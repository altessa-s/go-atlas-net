// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package netipx

import (
	"net/netip"
	"strconv"
	"testing"
)

func BenchmarkIsPrivate(b *testing.B) {
	addr := netip.MustParseAddr("10.0.0.1")
	for b.Loop() {
		IsPrivate(addr)
	}
}

func BenchmarkIsPrivate_Public(b *testing.B) {
	addr := netip.MustParseAddr("8.8.8.8")
	for b.Loop() {
		IsPrivate(addr)
	}
}

// benchPrefixes returns n distinct /24 prefixes in 10.0.0.0/8.
func benchPrefixes(n int) []netip.Prefix {
	out := make([]netip.Prefix, n)
	for i := range out {
		out[i] = netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 0}), 24)
	}
	return out
}

func BenchmarkIsTrusted(b *testing.B) {
	for _, n := range []int{1, 16, 256} {
		prefixes := benchPrefixes(n)
		last := netip.AddrFrom4([4]byte{10, byte((n - 1) >> 8), byte(n - 1), 7})
		b.Run("prefixes_"+strconv.Itoa(n)+"/last_match", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				IsTrusted(last, prefixes)
			}
		})
		b.Run("prefixes_"+strconv.Itoa(n)+"/miss_mapped", func(b *testing.B) {
			addr := netip.MustParseAddr("::ffff:8.8.8.8")
			b.ReportAllocs()
			for b.Loop() {
				IsTrusted(addr, prefixes)
			}
		})
	}
}

func BenchmarkInList(b *testing.B) {
	prefixes := benchPrefixes(64)
	addr := netip.MustParseAddr("10.0.63.1")
	b.ReportAllocs()
	for b.Loop() {
		InList(addr, prefixes)
	}
}

func BenchmarkParsePrefixes(b *testing.B) {
	strs := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.1.10", "2001:db8::/32", "::1", "100.64.0.0/10"}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParsePrefixes(strs); err != nil {
			b.Fatal(err)
		}
	}
}
