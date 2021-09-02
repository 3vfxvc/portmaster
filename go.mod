module github.com/safing/portmaster

go 1.15

require (
	github.com/agext/levenshtein v1.2.3
	github.com/cookieo9/resources-go v0.0.0-20150225115733-d27c04069d0d
	github.com/coreos/go-iptables v0.5.0
	github.com/florianl/go-nfqueue v1.2.0
	github.com/godbus/dbus/v5 v5.0.3
	github.com/google/gopacket v1.1.19
	github.com/hashicorp/go-multierror v1.1.0
	github.com/hashicorp/go-version v1.3.0
	github.com/leodido/go-urn v1.2.1 // indirect
	github.com/miekg/dns v1.1.40
	github.com/oschwald/maxminddb-golang v1.8.0
	github.com/refraction-networking/utls v0.0.0-20210713165636-0b2885c8c0d4
	github.com/safing/portbase v0.11.2
	github.com/safing/spn v0.2.5
	github.com/shirou/gopsutil v3.21.4+incompatible
	github.com/spf13/cobra v1.1.3
	github.com/stretchr/testify v1.6.1
	github.com/tannerryan/ring v1.1.2
	github.com/tevino/abool v1.2.0
	github.com/tidwall/gjson v1.7.5
	github.com/umahmood/haversine v0.0.0-20151105152445-808ab04add26
	golang.org/x/net v0.0.0-20210825183410-e898025ed96a
	golang.org/x/sync v0.0.0-20210220032951-036812b2e83c
	golang.org/x/sys v0.0.0-20210615035016-665e8c7367d1
)

replace github.com/safing/portbase => ../portbase

replace github.com/refraction-networking/utls => ../utls
