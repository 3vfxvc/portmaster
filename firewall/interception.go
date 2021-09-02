package firewall

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/google/gopacket/layers"
	"github.com/safing/portmaster/netenv"
	"golang.org/x/sync/singleflight"

	"github.com/tevino/abool"

	"github.com/safing/portbase/log"
	"github.com/safing/portbase/modules"
	"github.com/safing/portmaster/firewall/dpi"
	"github.com/safing/portmaster/firewall/inspection"
	"github.com/safing/portmaster/firewall/inspection/inspectutils"
	"github.com/safing/portmaster/firewall/interception"
	"github.com/safing/portmaster/network"
	"github.com/safing/portmaster/network/netutils"
	"github.com/safing/portmaster/network/packet"
	"github.com/safing/spn/captain"
	"github.com/safing/spn/sluice"

	// module dependencies
	_ "github.com/safing/portmaster/core/base"
)

var (
	interceptionModule *modules.Module

	nameserverIPMatcher      func(ip net.IP) bool
	nameserverIPMatcherSet   = abool.New()
	nameserverIPMatcherReady = abool.New()

	packetsAccepted = new(uint64)
	packetsBlocked  = new(uint64)
	packetsDropped  = new(uint64)
	packetsFailed   = new(uint64)

	blockedIPv4 = net.IPv4(0, 0, 0, 17)
	blockedIPv6 = net.ParseIP("::17")

	ownPID = os.Getpid()

	streamManager = dpi.NewManager()
)

func init() {
	interceptionModule = modules.Register("interception", interceptionPrep, interceptionStart, interceptionStop, "base", "updates", "network")

	network.SetDefaultFirewallHandler(defaultHandler)
}

func interceptionPrep() error {
	return prepAPIAuth()
}

func interceptionStart() error {
	if err := registerMetrics(); err != nil {
		return err
	}

	startAPIAuth()

	interceptionModule.StartWorker("stat logger", statLogger)
	interceptionModule.StartWorker("packet handler", packetHandler)

	return interception.Start()
}

func interceptionStop() error {
	return interception.Stop()
}

// SetNameserverIPMatcher sets a function that is used to match the internal
// nameserver IP(s). Can only bet set once.
func SetNameserverIPMatcher(fn func(ip net.IP) bool) error {
	if !nameserverIPMatcherSet.SetToIf(false, true) {
		return errors.New("nameserver IP matcher already set")
	}

	nameserverIPMatcher = fn
	nameserverIPMatcherReady.Set()
	return nil
}

func handlePacket(ctx context.Context, pkt packet.Packet) {
	// log.Errorf("DEBUG: firewall: handling packet %s", pkt)

	// Record metrics.
	startTime := time.Now()
	defer packetHandlingHistogram.UpdateDuration(startTime)

	if fastTrackedPermit(pkt) {
		return
	}

	// Add context tracer and set context on packet.
	traceCtx, tracer := log.AddTracer(ctx)
	if tracer != nil {
		// The trace is submitted in `network.Connection.packetHandler()`.
		tracer.Tracef("filter: handling packet: %s", pkt)
	}
	pkt.SetCtx(traceCtx)

	// Get connection of packet.
	conn, err := getConnection(pkt)
	if err != nil {
		tracer.Errorf("filter: packet %s dropped: %s", pkt, err)
		_ = pkt.Drop()
		return
	}

	// handle packet
	conn.HandlePacket(pkt)
}

var getConnectionSingleInflight singleflight.Group

func getConnection(pkt packet.Packet) (*network.Connection, error) {
	created := false

	// Create or get connection in single inflight lock in order to prevent duplicates.
	newConn, err, shared := getConnectionSingleInflight.Do(pkt.GetConnectionID(), func() (interface{}, error) {
		// First, check for an existing connection.
		conn, ok := network.GetConnection(pkt.GetConnectionID())
		if ok {
			return conn, nil
		}

		// Else create new one from the packet.
		conn = network.NewConnectionFromFirstPacket(pkt)
		conn.SetFirewallHandler(initialHandler)
		created = true
		return conn, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get connection: %s", err)
	}
	if newConn == nil {
		return nil, errors.New("connection getter returned nil")
	}

	// Transform and log result.
	conn := newConn.(*network.Connection)
	sharedIndicator := ""
	if shared {
		sharedIndicator = " (shared)"
	}
	if created {
		log.Tracer(pkt.Ctx()).Tracef("filter: created new connection %s%s", conn.ID, sharedIndicator)
	} else {
		log.Tracer(pkt.Ctx()).Tracef("filter: assigned connection %s%s", conn.ID, sharedIndicator)
	}

	return conn, nil
}

// fastTrackedPermit quickly permits certain network criticial or internal connections.
func fastTrackedPermit(pkt packet.Packet) (handled bool) {
	meta := pkt.Info()

	// Check if packed was already fast-tracked by the OS integration.
	if pkt.FastTrackedByIntegration() {
		log.Debugf("filter: fast-tracked by OS integration: %s", pkt)
		return true
	}

	// Check if connection was already blocked.
	if meta.Dst.Equal(blockedIPv4) || meta.Dst.Equal(blockedIPv6) {
		_ = pkt.PermanentBlock()
		return true
	}

	// Some programs do a network self-check where they connects to the same
	// IP/Port to test network capabilities.
	// Eg. dig: https://gitlab.isc.org/isc-projects/bind9/-/issues/1140
	if meta.SrcPort == meta.DstPort &&
		meta.Src.Equal(meta.Dst) {
		log.Debugf("filter: fast-track network self-check: %s", pkt)
		_ = pkt.PermanentAccept()
		return true
	}

	switch meta.Protocol {
	case packet.ICMP, packet.ICMPv6:
		// Load packet data.
		err := pkt.LoadPacketData()
		if err != nil {
			log.Debugf("filter: failed to load ICMP packet data: %s", err)
			_ = pkt.PermanentAccept()
			return true
		}

		// Handle echo request and replies regularly.
		// Other ICMP packets are considered system business.
		icmpLayers := pkt.Layers().LayerClass(layers.LayerClassIPControl)
		switch icmpLayer := icmpLayers.(type) {
		case *layers.ICMPv4:
			switch icmpLayer.TypeCode.Type() {
			case layers.ICMPv4TypeEchoRequest,
				layers.ICMPv4TypeEchoReply:
				return false
			}
		case *layers.ICMPv6:
			switch icmpLayer.TypeCode.Type() {
			case layers.ICMPv6TypeEchoRequest,
				layers.ICMPv6TypeEchoReply:
				return false
			}
		}

		// Premit all ICMP/v6 packets that are not echo requests or replies.
		log.Debugf("filter: fast-track accepting ICMP/v6: %s", pkt)

		// Submit to ICMP listener.
		submitted := netenv.SubmitPacketToICMPListener(pkt)

		// If the packet was submitted to the listener, we must not do a
		// permanent accept, because then we won't see any future packets of that
		// connection and thus cannot continue to submit them.
		if submitted {
			_ = pkt.Accept()
		} else {
			_ = pkt.PermanentAccept()
		}
		return true

	case packet.UDP, packet.TCP:
		switch meta.DstPort {

		case 67, 68, 546, 547:
			// Always allow DHCP, DHCPv6.

			// DHCP and DHCPv6 must be UDP.
			if meta.Protocol != packet.UDP {
				return false
			}

			// DHCP is only valid in local network scopes.
			switch netutils.ClassifyIP(meta.Dst) {
			case netutils.HostLocal, netutils.LinkLocal, netutils.SiteLocal, netutils.LocalMulticast:
			default:
				return false
			}

			// Log and permit.
			log.Debugf("filter: fast-track accepting DHCP: %s", pkt)
			_ = pkt.PermanentAccept()
			return true

		case apiPort:
			// Always allow direct access to the Portmaster API.

			// Portmaster API is TCP only.
			if meta.Protocol != packet.TCP {
				return false
			}

			// Check if the api port is even set.
			if !apiPortSet {
				return false
			}

			// Must be destined for the API IP.
			if !meta.Dst.Equal(apiIP) {
				return false
			}

			// Only fast-track local requests.
			isMe, err := netenv.IsMyIP(meta.Src)
			switch {
			case err != nil:
				log.Debugf("filter: failed to check if %s is own IP for fast-track: %s", meta.Src, err)
				return false
			case !isMe:
				return false
			}

			// Log and permit.
			log.Debugf("filter: fast-track accepting api connection: %s", pkt)
			_ = pkt.PermanentAccept()
			return true

		case 53:
			// Always allow direct access to the Portmaster Nameserver.
			// DNS is both UDP and TCP.

			// Check if a nameserver IP matcher is set.
			if !nameserverIPMatcherReady.IsSet() {
				return false
			}

			// Check if packet is destined for a nameserver IP.
			if !nameserverIPMatcher(meta.Dst) {
				return false
			}

			// Only fast-track local requests.
			isMe, err := netenv.IsMyIP(meta.Src)
			switch {
			case err != nil:
				log.Debugf("filter: failed to check if %s is own IP for fast-track: %s", meta.Src, err)
				return false
			case !isMe:
				return false
			}

			// Log and permit.
			log.Debugf("filter: fast-track accepting local dns: %s", pkt)
			_ = pkt.PermanentAccept()
			return true
		}
	}

	return false
}

func initialHandler(conn *network.Connection, pkt packet.Packet) {
	log.Tracer(pkt.Ctx()).Trace("filter: handing over to connection-based handler")

	// Check for pre-authenticated port.
	if !conn.Inbound && localPortIsPreAuthenticated(conn.Entity.Protocol, conn.LocalPort) {
		// Approve connection.
		conn.Accept("connection by Portmaster", noReasonOptionKey)
		conn.Internal = true
		// Finalize connection.
		conn.StopFirewallHandler()
		issueVerdict(conn, pkt, 0, true)
		return
	}

	// Redirect rogue dns requests to the Portmaster.
	if pkt.IsOutbound() &&
		pkt.Info().DstPort == 53 &&
		conn.Process().Pid != ownPID &&
		nameserverIPMatcherReady.IsSet() &&
		!nameserverIPMatcher(pkt.Info().Dst) {
		conn.Verdict = network.VerdictRerouteToNameserver
		conn.Reason.Msg = "redirecting rogue dns query"
		conn.Internal = true
		conn.StopFirewallHandler()
		issueVerdict(conn, pkt, 0, true)
		return
	}

	// check if filtering is enabled
	if !filterEnabled() {
		conn.Inspecting = false
		conn.Accept("privacy filter disabled", noReasonOptionKey)
		conn.StopFirewallHandler()
		issueVerdict(conn, pkt, 0, true)
		return
	}

	log.Tracer(pkt.Ctx()).Trace("filter: starting decision process")
	DecideOnConnection(pkt.Ctx(), conn, pkt)

	// tunneling
	// TODO: add implementation for forced tunneling
	if pkt.IsOutbound() &&
		captain.ClientReady() &&
		conn.Entity.IPScope.IsGlobal() &&
		conn.Verdict == network.VerdictAccept {
		// try to tunnel
		err := sluice.AwaitRequest(pkt.Info(), conn.Entity.Domain)
		if err != nil {
			log.Tracer(pkt.Ctx()).Tracef("filter: not tunneling: %s", err)
		} else {
			log.Tracer(pkt.Ctx()).Trace("filter: tunneling request")
			conn.Verdict = network.VerdictRerouteToTunnel
		}
	}

	switch {
	case conn.Inspecting:
		log.Tracer(pkt.Ctx()).Trace("filter: starting inspection")
		// setup inspectors and inspection handler
		inspection.InitializeInspectors(conn, pkt)
		conn.SetFirewallHandler(inspectThenVerdict)
		// execute inspection handler
		inspectThenVerdict(conn, pkt)
	default:
		conn.StopFirewallHandler()
		issueVerdict(conn, pkt, 0, true)
	}

}

func defaultHandler(conn *network.Connection, pkt packet.Packet) {
	// TODO: `pkt` has an active trace log, which we currently don't submit.
	issueVerdict(conn, pkt, 0, true)
}

func inspectThenVerdict(conn *network.Connection, pkt packet.Packet) {
	log.Infof("%s: inspecting ...", conn.ID)
	verdict, reason, err := streamManager.HandlePacket(conn, pkt)
	if err != nil {
		log.Tracer(pkt.Ctx()).Tracef("tcp-stream: failed to handle packet: %s", err)
		// temporarly accept the packet and retry the next time
		verdict = network.VerdictAccept
	}

	if verdict > network.VerdictUndecided {
		if reason == nil {
			reason = &inspectutils.Reason{}
		}
		conn.SetVerdict(verdict, reason.String(), "", reason.Context())
	}

	if !conn.Inspecting {
		log.Infof("stopping inspection of %s", conn.ID)
		conn.StopFirewallHandler()
		conn.SaveWhenFinished()
	}
	issueVerdict(conn, pkt, 0, !conn.Inspecting)

	/*
		pktVerdict, continueInspection := inspection.RunInspectors(conn, pkt)
		if continueInspection {
			issueVerdict(conn, pkt, pktVerdict, false)
			return
		}

		// we are done with inspecting
		conn.Inspecting = false
		conn.SaveWhenFinished()
		conn.StopFirewallHandler()
		issueVerdict(conn, pkt, 0, true)
	*/
}

func issueVerdict(conn *network.Connection, pkt packet.Packet, verdict network.Verdict, allowPermanent bool) {
	// enable permanent verdict
	if allowPermanent && !conn.VerdictPermanent {
		conn.VerdictPermanent = permanentVerdicts()
		if conn.VerdictPermanent {
			conn.SaveWhenFinished()
		}
	}

	// do not allow to circumvent decision: e.g. to ACCEPT packets from a DROP-ed connection
	if verdict < conn.Verdict {
		verdict = conn.Verdict
	}

	var err error
	switch verdict {
	case network.VerdictAccept:
		atomic.AddUint64(packetsAccepted, 1)
		if conn.VerdictPermanent {
			err = pkt.PermanentAccept()
		} else {
			err = pkt.Accept()
		}
	case network.VerdictBlock:
		atomic.AddUint64(packetsBlocked, 1)
		if conn.VerdictPermanent {
			err = pkt.PermanentBlock()
		} else {
			err = pkt.Block()
		}
	case network.VerdictDrop:
		atomic.AddUint64(packetsDropped, 1)
		if conn.VerdictPermanent {
			err = pkt.PermanentDrop()
		} else {
			err = pkt.Drop()
		}
	case network.VerdictRerouteToNameserver:
		err = pkt.RerouteToNameserver()
	case network.VerdictRerouteToTunnel:
		err = pkt.RerouteToTunnel()
	case network.VerdictFailed:
		atomic.AddUint64(packetsFailed, 1)
		err = pkt.Drop()
	default:
		atomic.AddUint64(packetsDropped, 1)
		err = pkt.Drop()
	}

	if err != nil {
		log.Warningf("filter: failed to apply verdict to pkt %s: %s", pkt, err)
	}
}

// func tunnelHandler(pkt packet.Packet) {
// 	tunnelInfo := GetTunnelInfo(pkt.Info().Dst)
// 	if tunnelInfo == nil {
// 		pkt.Block()
// 		return
// 	}
//
// 	entry.CreateTunnel(pkt, tunnelInfo.Domain, tunnelInfo.RRCache.ExportAllARecords())
// 	log.Tracef("filter: rerouting %s to tunnel entry point", pkt)
// 	pkt.RerouteToTunnel()
// 	return
// }

func packetHandler(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case pkt := <-interception.Packets:
			interceptionModule.StartWorker("initial packet handler", func(workerCtx context.Context) error {
				handlePacket(workerCtx, pkt)
				return nil
			})
		}
	}
}

func statLogger(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(10 * time.Second):
			log.Tracef(
				"filter: packets accepted %d, blocked %d, dropped %d, failed %d",
				atomic.LoadUint64(packetsAccepted),
				atomic.LoadUint64(packetsBlocked),
				atomic.LoadUint64(packetsDropped),
				atomic.LoadUint64(packetsFailed),
			)
			atomic.StoreUint64(packetsAccepted, 0)
			atomic.StoreUint64(packetsBlocked, 0)
			atomic.StoreUint64(packetsDropped, 0)
			atomic.StoreUint64(packetsFailed, 0)
		}
	}
}
