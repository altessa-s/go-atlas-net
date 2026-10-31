// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

// Package netipx classifies IP addresses and matches them against prefix
// lists, on top of the standard [net/netip] package.
//
// IsPrivate reports whether an address is private, loopback, link-local or
// otherwise reserved. It checks an IPv4-mapped IPv6 address and a well-known
// NAT64 address as the IPv4 address they reach and ignores an IPv6 zone, so
// none of these forms slips past the reserved ranges. The outbound SSRF
// guard of net/http/client and the inbound IP ACLs and client IP extraction
// share it, so an address is classified the same way in both directions.
//
// IsTrusted and InList match an address exactly as given against a prefix
// list; ParsePrefixes parses CIDR and single-address strings into prefixes.
//
// Example:
//
//	prefixes, err := netipx.ParsePrefixes([]string{"10.0.0.0/8", "192.168.1.1"})
//	if err != nil {
//		return err
//	}
//	if netipx.IsPrivate(addr) || netipx.InList(addr, prefixes) {
//		// reject
//	}
package netipx
