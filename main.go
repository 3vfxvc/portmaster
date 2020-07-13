package main

import (
	"os"

	"github.com/safing/portbase/info"
	"github.com/safing/portbase/run"

	// include packages here
	_ "github.com/safing/portbase/modules/subsystems"
	_ "github.com/safing/portmaster/core"
	_ "github.com/safing/portmaster/firewall"
	_ "github.com/safing/portmaster/firewall/inspection/encryption"
	_ "github.com/safing/portmaster/nameserver"
	_ "github.com/safing/portmaster/ui"
)

func main() {
	info.Set("Portmaster", "0.4.10", "AGPLv3", true)
	os.Exit(run.Run())
}
