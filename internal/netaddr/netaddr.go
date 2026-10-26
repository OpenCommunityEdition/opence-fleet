// Package netaddr says which addresses are globally routable (an
// allowlist), and how addresses are grouped for rate limits (an IPv4
// address alone, an IPv6 address by its /64). It is the master server's
// package.
package netaddr

import (
	"net/netip"
)

func prefixes(texts ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(texts))
	for i, text := range texts {
		out[i] = netip.MustParsePrefix(text)
	}
	return out
}

// the IPv4 ranges that are not globally routable (IANA's special-purpose
// registry, RFC 6890 and its updates)
var notGlobal4 = prefixes(
	"0.0.0.0/8",       // "this network"
	"10.0.0.0/8",      // private
	"100.64.0.0/10",   // shared address space (carrier-grade NAT)
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link-local
	"172.16.0.0/12",   // private
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation (TEST-NET-1)
	"192.88.99.0/24",  // the deprecated 6to4 relay anycast
	"192.168.0.0/16",  // private
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // documentation (TEST-NET-2)
	"203.0.113.0/24",  // documentation (TEST-NET-3)
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved, and the limited broadcast address
)

// the IPv6 global unicast space (2000::/3) is global but for these
var notGlobal6 = prefixes(
	"2001::/23",     // IETF protocol assignments (Teredo, ORCHID, benchmarking, ...)
	"2001:db8::/32", // documentation
	"2002::/16",     // 6to4: global only as the IPv4 address it embeds (below)
	"3fff::/20",     // documentation (RFC 9637)
	"5f00::/16",     // SRv6 segment identifiers (RFC 9602)
)

var (
	globalUnicast6 = netip.MustParsePrefix("2000::/3")
	sixToFour      = netip.MustParsePrefix("2002::/16")
	nat64          = netip.MustParsePrefix("64:ff9b::/96")
)

func in(address netip.Addr, list []netip.Prefix) bool {
	for _, p := range list {
		if p.Contains(address) {
			return true
		}
	}
	return false
}

// Global tells whether an address is globally routable unicast: an IPv4
// address outside the special-purpose ranges; an IPv6 address in 2000::/3
// outside its special-purpose ranges, a 6to4 address (2002::/16) or a
// well-known NAT64 address (64:ff9b::/96) whose embedded IPv4 address is
// global. An IPv4-mapped IPv6 address is judged as its IPv4 address.
// Everything else (loopback, private, link-local, site-local fec0::/10,
// unique local fc00::/7, multicast, documentation, the local-use NAT64
// 64:ff9b:1::/48, zones) is not.
func Global(address netip.Addr) bool {
	if !address.IsValid() || address.Zone() != "" {
		return false
	}
	address = address.Unmap()
	if address.Is4() {
		return !in(address, notGlobal4)
	}
	bytes := address.As16()
	switch {
	case nat64.Contains(address):
		return Global(netip.AddrFrom4([4]byte(bytes[12:16])))
	case sixToFour.Contains(address):
		return Global(netip.AddrFrom4([4]byte(bytes[2:6])))
	case !globalUnicast6.Contains(address):
		return false
	}
	return !in(address, notGlobal6)
}

// Local tells whether an address is loopback or private (RFC 1918, unique
// local).
func Local(address netip.Addr) bool {
	address = address.Unmap()
	return address.IsLoopback() || address.IsPrivate()
}

// Key is the key an address's requests are counted under: an IPv4 address
// itself, an IPv6 address's /64 (one subscriber's usual allocation), so that
// a host cannot pass a limit by changing the low bits of its address.
func Key(address netip.Addr) string {
	address = address.Unmap().WithZone("")
	if !address.IsValid() {
		return ""
	}
	if address.Is4() {
		return address.String()
	}
	return netip.PrefixFrom(address, 64).Masked().String()
}

// Group is the group a key of Key counts in for a limiter's memory: an IPv6
// /64's /48 (a site's usual allocation, 65,536 /64s), so that one site's
// keys make room among themselves; "" for an IPv4 address and for a key
// that is not an address's.
func Group(key string) string {
	prefix, err := netip.ParsePrefix(key)
	if err != nil || !prefix.Addr().Is6() || prefix.Bits() < 48 {
		return ""
	}
	return netip.PrefixFrom(prefix.Addr(), 48).Masked().String()
}
