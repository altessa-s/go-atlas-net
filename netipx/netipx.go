// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package netipx

import (
	"net/netip"
	"slices"
)

// privateAndLocalRanges contains loopback, private, link local, and default unicast prefixes.
var privateAndLocalRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),         // RFC1918
	netip.MustParsePrefix("172.16.0.0/12"),      // private
	netip.MustParsePrefix("192.168.0.0/16"),     // private
	netip.MustParsePrefix("127.0.0.0/8"),        // RFC5735
	netip.MustParsePrefix("0.0.0.0/8"),          // RFC1122 Section 3.2.1.3
	netip.MustParsePrefix("169.254.0.0/16"),     // RFC3927
	netip.MustParsePrefix("192.0.0.0/24"),       // RFC 5736
	netip.MustParsePrefix("192.0.2.0/24"),       // RFC 5737
	netip.MustParsePrefix("198.51.100.0/24"),    // Assigned as TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),     // Assigned as TEST-NET-3
	netip.MustParsePrefix("192.88.99.0/24"),     // RFC 3068
	netip.MustParsePrefix("198.18.0.0/15"),      // RFC 2544: Benchmarking
	netip.MustParsePrefix("224.0.0.0/4"),        // RFC 3171
	netip.MustParsePrefix("240.0.0.0/4"),        // RFC 1112
	netip.MustParsePrefix("255.255.255.255/32"), // RFC 919 Section 7
	netip.MustParsePrefix("100.64.0.0/10"),      // RFC 6598
	netip.MustParsePrefix("::/128"),             // RFC 4291: Unspecified Address
	netip.MustParsePrefix("::1/128"),            // RFC 4291: Loopback Address
	netip.MustParsePrefix("100::/64"),           // RFC 6666: Discard Address Block
	netip.MustParsePrefix("2001::/23"),          // RFC 2928: IETF Protocol Assignments
	netip.MustParsePrefix("2001:2::/48"),        // RFC 5180: Benchmarking
	netip.MustParsePrefix("2001:db8::/32"),      // RFC 3849: Documentation
	netip.MustParsePrefix("2001::/32"),          // RFC 4380: TEREDO
	netip.MustParsePrefix("fc00::/7"),           // RFC 4193: Unique-Local
	netip.MustParsePrefix("fe80::/10"),          // RFC 4291: Section 2.5.6 Link-Scoped Unicast
	netip.MustParsePrefix("ff00::/8"),           // RFC 4291: Section 2.7
	netip.MustParsePrefix("2002::/16"),          // RFC 7526: 6to4 anycast prefix deprecated
	netip.MustParsePrefix("::/96"),              // RFC 4291: deprecated IPv4-compatible addresses
	netip.MustParsePrefix("64:ff9b:1::/48"),     // RFC 8215: local-use IPv4/IPv6 translation
	netip.MustParsePrefix("3fff::/20"),          // RFC 9637: Documentation
	netip.MustParsePrefix("5f00::/16"),          // RFC 9602: SRv6 SIDs
	netip.MustParsePrefix("fec0::/10"),          // RFC 3879: deprecated site-local
}

// nat64WellKnownPrefix is the RFC 6052 well-known NAT64 prefix. An address in
// it reaches the IPv4 address held in its last 32 bits, so it is classified as
// that IPv4 address.
var nat64WellKnownPrefix = netip.MustParsePrefix("64:ff9b::/96")

// IsPrivate reports whether ip falls within any RFC-defined private, loopback,
// link-local, or reserved address range. The full list covers IPv4 and IPv6
// ranges from RFC 1918, RFC 4193, RFC 5735, and others.
//
// An IPv4-mapped IPv6 address (::ffff:a.b.c.d) is checked as the IPv4 address
// it reaches, and an IPv6 zone (fe80::1%en0) is ignored: a zone selects an
// interface, not a different network, and a zoned address never matches a
// prefix on its own. A NAT64 address in the well-known prefix 64:ff9b::/96
// is checked as the IPv4 address it translates to, so 64:ff9b::a9fe:a9fe is
// private (169.254.169.254) and 64:ff9b::808:808 is not (8.8.8.8).
func IsPrivate(ip netip.Addr) bool {
	ip = ip.Unmap().WithZone("")
	if nat64WellKnownPrefix.Contains(ip) {
		b := ip.As16()
		ip = netip.AddrFrom4([4]byte(b[12:]))
	}
	return InList(ip, privateAndLocalRanges)
}

// IsTrusted reports whether ip belongs to any of the given trusted network
// prefixes. An empty trustedPrefixes slice always returns false.
func IsTrusted(ip netip.Addr, trustedPrefixes []netip.Prefix) bool {
	return InList(ip, trustedPrefixes)
}

// InList reports whether ip is contained in any of the given CIDR prefixes.
func InList(ip netip.Addr, prefixes []netip.Prefix) bool {
	return slices.ContainsFunc(prefixes, func(prefix netip.Prefix) bool {
		return prefix.Contains(ip)
	})
}
