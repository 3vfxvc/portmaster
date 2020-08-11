package resolver

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/safing/portbase/log"
	"github.com/safing/portbase/modules"
	"github.com/safing/portmaster/intel"

	// module dependencies
	_ "github.com/safing/portmaster/core/base"
)

var (
	// ClearNameCacheEvent is a triggerable event that clears the name record cache.
	ClearNameCacheEvent = "clear name cache"

	module *modules.Module
)

func init() {
	module = modules.Register("resolver", prep, start, nil, "base", "netenv")
	module.RegisterEvent(ClearNameCacheEvent)
}

func prep() error {
	intel.SetReverseResolver(ResolveIPAndValidate)

	if err := prepEnvResolver(); err != nil {
		return err
	}

	return prepConfig()
}

func start() error {
	// load resolvers from config and environment
	loadResolvers()

	// reload after network change
	err := module.RegisterEventHook(
		"netenv",
		"network changed",
		"update nameservers",
		func(_ context.Context, _ interface{}) error {
			loadResolvers()
			log.Debug("resolver: reloaded nameservers due to network change")
			return nil
		},
	)
	if err != nil {
		return err
	}

	// reload after config change
	prevNameservers := strings.Join(configuredNameServers(), " ")
	err = module.RegisterEventHook(
		"config",
		"config change",
		"update nameservers",
		func(_ context.Context, _ interface{}) error {
			newNameservers := strings.Join(configuredNameServers(), " ")
			if newNameservers != prevNameservers {
				prevNameservers = newNameservers

				loadResolvers()
				log.Debug("resolver: reloaded nameservers due to config change")
			}
			return nil
		},
	)
	if err != nil {
		return err
	}

	// cache clearing
	err = module.RegisterEventHook(
		"resolver",
		ClearNameCacheEvent,
		ClearNameCacheEvent,
		clearNameCache,
	)
	if err != nil {
		return err
	}

	module.StartServiceWorker(
		"mdns handler",
		5*time.Second,
		listenToMDNS,
	)

	return nil
}

var (
	localAddrFactory func(network string) net.Addr
)

// SetLocalAddrFactory supplies the intel package with a function to get permitted local addresses for connections.
func SetLocalAddrFactory(laf func(network string) net.Addr) {
	if localAddrFactory == nil {
		localAddrFactory = laf
	}
}

func getLocalAddr(network string) net.Addr {
	if localAddrFactory != nil {
		return localAddrFactory(network)
	}
	return nil
}
