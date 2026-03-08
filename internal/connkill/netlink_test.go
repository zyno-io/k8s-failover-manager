//go:build linux

package connkill

import (
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMatchesDest_IPv4Match(t *testing.T) {
	ip := net.ParseIP("10.0.0.1")
	ip4 := ip.To4()

	msg := inetDiagMsg{}
	msg.ID.Dst[0] = ip4[0]
	msg.ID.Dst[1] = ip4[1]
	msg.ID.Dst[2] = ip4[2]
	msg.ID.Dst[3] = ip4[3]

	if !matchesDest(msg, ip, unix.AF_INET) {
		t.Error("expected match for matching IPv4")
	}
}

func TestMatchesDest_IPv4NoMatch(t *testing.T) {
	destIP := net.ParseIP("10.0.0.1")

	msg := inetDiagMsg{}
	other := net.ParseIP("10.0.0.2").To4()
	msg.ID.Dst[0] = other[0]
	msg.ID.Dst[1] = other[1]
	msg.ID.Dst[2] = other[2]
	msg.ID.Dst[3] = other[3]

	if matchesDest(msg, destIP, unix.AF_INET) {
		t.Error("expected no match for different IPv4")
	}
}

func TestMatchesDest_IPv6Match(t *testing.T) {
	ip := net.ParseIP("fd00::1")
	ip6 := ip.To16()

	msg := inetDiagMsg{}
	copy(msg.ID.Dst[:], ip6)

	if !matchesDest(msg, ip, unix.AF_INET6) {
		t.Error("expected match for matching IPv6")
	}
}

func TestMatchesDest_IPv6NoMatch(t *testing.T) {
	destIP := net.ParseIP("fd00::1")

	msg := inetDiagMsg{}
	other := net.ParseIP("fd00::2").To16()
	copy(msg.ID.Dst[:], other)

	if matchesDest(msg, destIP, unix.AF_INET6) {
		t.Error("expected no match for different IPv6")
	}
}

func TestMatchesDest_IPv4NilConversion(t *testing.T) {
	// An IPv6 address passed with AF_INET family — To4() returns nil
	ip := net.ParseIP("fd00::1")
	msg := inetDiagMsg{}

	if matchesDest(msg, ip, unix.AF_INET) {
		t.Error("expected false when IPv6 addr used with AF_INET family")
	}
}

func TestNativeEndian_Initialized(t *testing.T) {
	if nativeEndian == nil {
		t.Fatal("expected nativeEndian to be initialized after init()")
	}
}
