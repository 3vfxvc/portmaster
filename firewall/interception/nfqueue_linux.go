package interception

import (
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/hashicorp/go-multierror"

	"github.com/safing/portbase/log"
	"github.com/safing/portmaster/firewall/interception/nfqexp"
	"github.com/safing/portmaster/firewall/interception/nfqueue"
	"github.com/safing/portmaster/network/packet"
)

// iptables -A OUTPUT -p icmp -j", "NFQUEUE", "--queue-num", "1", "--queue-bypass

var (
	v4chains []string
	v4rules  []string
	v4once   []string

	v6chains []string
	v6rules  []string
	v6once   []string

	out4Queue nfQueue
	in4Queue  nfQueue
	out6Queue nfQueue
	in6Queue  nfQueue

	shutdownSignal = make(chan struct{})

	experimentalNfqueueBackend bool
)

func init() {
	flag.BoolVar(&experimentalNfqueueBackend, "experimental-nfqueue", false, "use experimental nfqueue packet")
}

// nfQueueFactoryFunc creates a new nfQueue with qid as the queue number.
type nfQueueFactoryFunc func(qid uint16, v6 bool) (nfQueue, error)

// nfQueue encapsulates nfQueue providers
type nfQueue interface {
	PacketChannel() <-chan packet.Packet
	Destroy()
}

func init() {

	v4chains = []string{
		"mangle C170",
		"mangle C171",
		"filter C17",
	}

	v4rules = []string{
		"mangle C170 -j CONNMARK --restore-mark",
		"mangle C170 -m mark --mark 0 -j NFQUEUE --queue-num 17040 --queue-bypass",

		"mangle C171 -j CONNMARK --restore-mark",
		"mangle C171 -m mark --mark 0 -j NFQUEUE --queue-num 17140 --queue-bypass",

		"filter C17 -m mark --mark 0 -j DROP",
		"filter C17 -m mark --mark 1700 -j RETURN",
		"filter C17 -m mark --mark 1701 -j REJECT --reject-with icmp-host-prohibited",
		"filter C17 -m mark --mark 1702 -j DROP",
		"filter C17 -j CONNMARK --save-mark",
		"filter C17 -m mark --mark 1710 -j RETURN",
		"filter C17 -m mark --mark 1711 -j REJECT --reject-with icmp-host-prohibited",
		"filter C17 -m mark --mark 1712 -j DROP",
		"filter C17 -m mark --mark 1717 -j RETURN",
	}

	v4once = []string{
		"mangle OUTPUT -j C170",
		"mangle INPUT -j C171",
		"filter OUTPUT -j C17",
		"filter INPUT -j C17",
		"nat OUTPUT -m mark --mark 1799 -p udp -j DNAT --to 127.0.0.17:53",
		"nat OUTPUT -m mark --mark 1717 -p tcp -j DNAT --to 127.0.0.17:717",
		"nat OUTPUT -m mark --mark 1717 -p udp -j DNAT --to 127.0.0.17:717",
		// "nat OUTPUT -m mark --mark 1717 ! -p tcp ! -p udp -j DNAT --to 127.0.0.17",
	}

	v6chains = []string{
		"mangle C170",
		"mangle C171",
		"filter C17",
	}

	v6rules = []string{
		"mangle C170 -j CONNMARK --restore-mark",
		"mangle C170 -m mark --mark 0 -j NFQUEUE --queue-num 17060 --queue-bypass",

		"mangle C171 -j CONNMARK --restore-mark",
		"mangle C171 -m mark --mark 0 -j NFQUEUE --queue-num 17160 --queue-bypass",

		"filter C17 -m mark --mark 0 -j DROP",
		"filter C17 -m mark --mark 1700 -j RETURN",
		"filter C17 -m mark --mark 1701 -j REJECT --reject-with icmp6-adm-prohibited",
		"filter C17 -m mark --mark 1702 -j DROP",
		"filter C17 -j CONNMARK --save-mark",
		"filter C17 -m mark --mark 1710 -j RETURN",
		"filter C17 -m mark --mark 1711 -j REJECT --reject-with icmp6-adm-prohibited",
		"filter C17 -m mark --mark 1712 -j DROP",
		"filter C17 -m mark --mark 1717 -j RETURN",
	}

	v6once = []string{
		"mangle OUTPUT -j C170",
		"mangle INPUT -j C171",
		"filter OUTPUT -j C17",
		"filter INPUT -j C17",
		"nat OUTPUT -m mark --mark 1799 -p udp -j DNAT --to [fd17::17]:53",
		"nat OUTPUT -m mark --mark 1717 -p tcp -j DNAT --to [fd17::17]:717",
		"nat OUTPUT -m mark --mark 1717 -p udp -j DNAT --to [fd17::17]:717",
		// "nat OUTPUT -m mark --mark 1717 ! -p tcp ! -p udp -j DNAT --to [fd17::17]",
	}

	// Reverse because we'd like to insert in a loop
	_ = sort.Reverse(sort.StringSlice(v4once)) // silence vet (sort is used just like in the docs)
	_ = sort.Reverse(sort.StringSlice(v6once)) // silence vet (sort is used just like in the docs)

}

func activateNfqueueFirewall() error {
	if err := activateIPTables(iptables.ProtocolIPv4, v4rules, v4once, v4chains); err != nil {
		return err
	}

	if err := activateIPTables(iptables.ProtocolIPv6, v6rules, v6once, v6chains); err != nil {
		return err
	}

	return nil
}

// DeactivateNfqueueFirewall drops portmaster related IP tables rules.
// Any errors encountered accumulated into a *multierror.Error.
func DeactivateNfqueueFirewall() error {
	// IPv4
	var result *multierror.Error
	if err := deactivateIPTables(iptables.ProtocolIPv4, v4once, v4chains); err != nil {
		result = multierror.Append(result, err)
	}

	// IPv6
	if err := deactivateIPTables(iptables.ProtocolIPv6, v6once, v6chains); err != nil {
		result = multierror.Append(result, err)
	}

	return result.ErrorOrNil()
}

func activateIPTables(protocol iptables.Protocol, rules, once, chains []string) error {
	tbls, err := iptables.NewWithProtocol(protocol)
	if err != nil {
		return err
	}

	for _, chain := range chains {
		splittedRule := strings.Split(chain, " ")
		if err = tbls.ClearChain(splittedRule[0], splittedRule[1]); err != nil {
			return err
		}
	}

	for _, rule := range rules {
		splittedRule := strings.Split(rule, " ")
		if err = tbls.Append(splittedRule[0], splittedRule[1], splittedRule[2:]...); err != nil {
			return err
		}
	}

	for _, rule := range once {
		splittedRule := strings.Split(rule, " ")
		ok, err := tbls.Exists(splittedRule[0], splittedRule[1], splittedRule[2:]...)
		if err != nil {
			return err
		}
		if !ok {
			if err = tbls.Insert(splittedRule[0], splittedRule[1], 1, splittedRule[2:]...); err != nil {
				return err
			}
		}
	}

	return nil
}

func deactivateIPTables(protocol iptables.Protocol, rules, chains []string) error {
	tbls, err := iptables.NewWithProtocol(protocol)
	if err != nil {
		return err
	}

	var multierr *multierror.Error

	for _, rule := range rules {
		splittedRule := strings.Split(rule, " ")
		ok, err := tbls.Exists(splittedRule[0], splittedRule[1], splittedRule[2:]...)
		if err != nil {
			multierr = multierror.Append(multierr, err)
		}
		if ok {
			if err = tbls.Delete(splittedRule[0], splittedRule[1], splittedRule[2:]...); err != nil {
				multierr = multierror.Append(multierr, err)
			}
		}
	}

	for _, chain := range chains {
		splittedRule := strings.Split(chain, " ")
		if err = tbls.ClearChain(splittedRule[0], splittedRule[1]); err != nil {
			multierr = multierror.Append(multierr, err)
		}
		if err = tbls.DeleteChain(splittedRule[0], splittedRule[1]); err != nil {
			multierr = multierror.Append(multierr, err)
		}
	}

	return multierr.ErrorOrNil()
}

// StartNfqueueInterception starts the nfqueue interception.
func StartNfqueueInterception() (err error) {
	var nfQueueFactory nfQueueFactoryFunc = func(qid uint16, v6 bool) (nfQueue, error) {
		return nfqueue.NewNFQueue(qid)
	}

	if experimentalNfqueueBackend {
		log.Infof("nfqueue: using experimental nfqueue backend")
		nfQueueFactory = func(qid uint16, v6 bool) (nfQueue, error) {
			return nfqexp.New(qid, v6)
		}
	}

	err = activateNfqueueFirewall()
	if err != nil {
		_ = Stop()
		return fmt.Errorf("could not initialize nfqueue: %s", err)
	}

	out4Queue, err = nfQueueFactory(17040, false)
	if err != nil {
		_ = Stop()
		return fmt.Errorf("nfqueue(IPv4, out): %w", err)
	}
	in4Queue, err = nfQueueFactory(17140, false)
	if err != nil {
		_ = Stop()
		return fmt.Errorf("nfqueue(IPv4, in): %w", err)
	}
	out6Queue, err = nfQueueFactory(17060, true)
	if err != nil {
		_ = Stop()
		return fmt.Errorf("nfqueue(IPv6, out): %w", err)
	}
	in6Queue, err = nfQueueFactory(17160, true)
	if err != nil {
		_ = Stop()
		return fmt.Errorf("nfqueue(IPv6, in): %w", err)
	}

	go handleInterception()
	return nil
}

// StopNfqueueInterception stops the nfqueue interception.
func StopNfqueueInterception() error {
	defer close(shutdownSignal)

	if out4Queue != nil {
		out4Queue.Destroy()
	}
	if in4Queue != nil {
		in4Queue.Destroy()
	}
	if out6Queue != nil {
		out6Queue.Destroy()
	}
	if in6Queue != nil {
		in6Queue.Destroy()
	}

	err := DeactivateNfqueueFirewall()
	if err != nil {
		return fmt.Errorf("interception: error while deactivating nfqueue: %s", err)
	}

	return nil
}

func handleInterception() {
	for {
		select {
		case <-shutdownSignal:
			return
		case pkt := <-out4Queue.PacketChannel():
			pkt.SetOutbound()
			Packets <- pkt
		case pkt := <-in4Queue.PacketChannel():
			pkt.SetInbound()
			Packets <- pkt
		case pkt := <-out6Queue.PacketChannel():
			pkt.SetOutbound()
			Packets <- pkt
		case pkt := <-in6Queue.PacketChannel():
			pkt.SetInbound()
			Packets <- pkt
		}
	}
}
