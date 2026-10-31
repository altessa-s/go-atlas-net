// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package clientip

import (
	"net/netip"
	"testing"
)

func BenchmarkExtractor_Extract_UntrustedPeer(b *testing.B) {
	ext, _ := NewExtractor()
	peerIP := netip.MustParseAddr("8.8.8.8")
	headers := &benchHeaders{}
	ctx := b.Context()

	for b.Loop() {
		ext.Extract(ctx, peerIP, headers)
	}
}

func BenchmarkExtractor_Extract_WithHeaders(b *testing.B) {
	ext, _ := NewExtractor(WithTrustedPeers(netip.MustParsePrefix("10.0.0.0/8")))
	peerIP := netip.MustParseAddr("10.0.0.1")
	headers := &benchHeaders{data: map[string][]string{
		HeaderXForwardedFor: {"203.0.114.1"},
	}}
	ctx := b.Context()

	for b.Loop() {
		ext.Extract(ctx, peerIP, headers)
	}
}

type benchHeaders struct {
	data map[string][]string
}

func (m *benchHeaders) GetHeader(name string) []string {
	return m.data[name]
}

// benchChainCount is the number of distinct X-Forwarded-For chains cycled by
// the distinct-chain benchmarks: enough that no per-entry cache can hold them.
const benchChainCount = 1 << 16

// benchChains returns benchChainCount distinct three-entry chains, built up
// front so the benchmark measures extraction, not header construction.
func benchChains(tb testing.TB) []*benchHeaders {
	tb.Helper()
	chains := make([]*benchHeaders, benchChainCount)
	for i := range chains {
		hi, lo := byte(i>>8), byte(i)
		chain := netip.AddrFrom4([4]byte{198, 51, hi, lo}).String() + ", " +
			netip.AddrFrom4([4]byte{10, 1, hi, lo}).String() + ", " +
			netip.AddrFrom4([4]byte{10, 2, hi, lo}).String()
		chains[i] = &benchHeaders{data: map[string][]string{HeaderXForwardedFor: {chain}}}
	}
	return chains
}

func BenchmarkExtractor_Extract_FixedChain(b *testing.B) {
	ext, _ := NewExtractor(WithTrustedProxies(netip.MustParsePrefix("10.0.0.0/8")))
	peerIP := netip.MustParseAddr("10.0.0.1")
	headers := &benchHeaders{data: map[string][]string{
		HeaderXForwardedFor: {"198.51.100.7, 10.1.0.1, 10.2.0.1"},
	}}
	ctx := b.Context()

	for b.Loop() {
		ext.Extract(ctx, peerIP, headers)
	}
}

func BenchmarkExtractor_Extract_DistinctChains(b *testing.B) {
	ext, _ := NewExtractor(WithTrustedProxies(netip.MustParsePrefix("10.0.0.0/8")))
	peerIP := netip.MustParseAddr("10.0.0.1")
	chains := benchChains(b)
	ctx := b.Context()

	i := 0
	for b.Loop() {
		ext.Extract(ctx, peerIP, chains[i%benchChainCount])
		i++
	}
}
