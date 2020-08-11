package netutils

import (
	"net"
	"testing"
)

func TestIPClassification(t *testing.T) {
	testClassification(t, net.IPv4(71, 87, 113, 211), Global)
	testClassification(t, net.IPv4(127, 0, 0, 1), HostLocal)
	testClassification(t, net.IPv4(127, 255, 255, 1), HostLocal)
	testClassification(t, net.IPv4(192, 168, 172, 24), SiteLocal)
	testClassification(t, net.IPv4(172, 15, 1, 1), Global)
	testClassification(t, net.IPv4(172, 16, 1, 1), SiteLocal)
	testClassification(t, net.IPv4(172, 31, 1, 1), SiteLocal)
	testClassification(t, net.IPv4(172, 32, 1, 1), Global)
}

func testClassification(t *testing.T, ip net.IP, expectedClassification int8) {
	c := ClassifyIP(ip)
	if c != expectedClassification {
		t.Errorf("%s is %s, expected %s", ip, classificationString(c), classificationString(expectedClassification))
	}
}

func classificationString(c int8) string {
	switch c {
	case HostLocal:
		return "hostLocal"
	case LinkLocal:
		return "linkLocal"
	case SiteLocal:
		return "siteLocal"
	case Global:
		return "global"
	case LocalMulticast:
		return "localMulticast"
	case GlobalMulticast:
		return "globalMulticast"
	case Invalid:
		return "invalid"
	default:
		return "unknown"
	}
}
