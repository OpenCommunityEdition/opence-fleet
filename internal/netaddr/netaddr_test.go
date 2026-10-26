package netaddr

import (
	"net/netip"
	"testing"
)

func TestGlobal(t *testing.T) {
	for _, text := range []string{
		"1.1.1.1", "8.8.8.8", "93.184.216.34", "100.63.255.255", "100.128.0.0", "192.0.1.255", "198.17.255.255",
		"198.20.0.0", "223.255.255.255", "::ffff:8.8.8.8",
		"2606:4700:4700::1111", "2a00:1450:4001:82a::200e", "2001:4860:4860::8888", "64:ff9b::808:808",
		"2002:808:808::1",
	} {
		if !Global(netip.MustParseAddr(text)) {
			t.Errorf("%s refused", text)
		}
	}
	for _, text := range []string{
		"0.0.0.0", "0.1.2.3", "10.1.2.3", "100.64.0.1", "100.127.255.255", "127.0.0.1", "169.254.1.1",
		"172.16.0.1", "172.31.255.255", "192.0.0.8", "192.0.0.170", "192.0.2.1", "192.88.99.1", "192.168.1.1",
		"198.18.0.1", "198.19.255.255", "198.51.100.7", "203.0.113.5", "224.0.0.1", "239.255.255.250",
		"240.0.0.1", "255.255.255.255", "::ffff:10.0.0.1", "::ffff:100.64.0.1",
		"::", "::1", "fe80::1", "fe80::1%eth0", "fec0::1", "fc00::1", "fd12:3456::1", "ff02::1",
		"2001:db8::1", "2001::1", "2001:2::1", "2001:10::1", "3fff::1", "5f00::1", "100::1",
		"64:ff9b::a00:1", "64:ff9b::7f00:1", "64:ff9b:1::808:808", "2002:a00:1::1", "2002:c0a8:101::1",
		"2002:6440:1::1", "4000::1", "::8.8.8.8",
	} {
		if Global(netip.MustParseAddr(text)) {
			t.Errorf("%s accepted", text)
		}
	}
	if Global(netip.Addr{}) {
		t.Error("the zero address accepted")
	}
}

func TestKey(t *testing.T) {
	for text, want := range map[string]string{
		"203.0.113.5":                    "203.0.113.5",
		"::ffff:203.0.113.5":             "203.0.113.5",
		"2001:db8:1:2:3:4:5:6":           "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff:ffff:ffff:ff": "2001:db8:1:2::/64",
		"fe80::1%eth0":                   "fe80::/64",
	} {
		if got := Key(netip.MustParseAddr(text)); got != want {
			t.Errorf("%s: %s, want %s", text, got, want)
		}
	}
	if Key(netip.Addr{}) != "" {
		t.Error("the zero address has a key")
	}
}

func TestGroup(t *testing.T) {
	for key, want := range map[string]string{
		Key(netip.MustParseAddr("2001:db8:1:2::1")):    "2001:db8:1::/48",
		Key(netip.MustParseAddr("2001:db8:1:ffff::1")): "2001:db8:1::/48",
		Key(netip.MustParseAddr("2001:db8:2:2::1")):    "2001:db8:2::/48",
		"203.0.113.5":      "",
		"0123456789abcdef": "",
		"":                 "",
	} {
		if got := Group(key); got != want {
			t.Errorf("%q: %q, want %q", key, got, want)
		}
	}
}
