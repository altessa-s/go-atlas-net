// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package clientip

import (
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/altessa-s/go-atlas-net/httpx"
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

// benchChain returns an X-Forwarded-For value of n public client-supplied
// entries followed by the given proxy hops.
func benchChain(n int, hops ...string) string {
	parts := make([]string, 0, n+len(hops))
	for i := range n {
		parts = append(parts, netip.AddrFrom4([4]byte{1, 0, byte(i >> 8), byte(i)}).String())
	}
	return strings.Join(append(parts, hops...), ", ")
}

// BenchmarkExtractor_Extract_LongChains measures X-Forwarded-For chains a
// client can pad at will: only the retained right end may cost anything.
func BenchmarkExtractor_Extract_LongChains(b *testing.B) {
	peer := netip.MustParseAddr("10.0.0.1")
	trusted := WithTrustedProxies(netip.MustParsePrefix("10.0.0.0/8"))
	hops := []string{"10.1.0.1", "10.2.0.1"}
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = benchChain(5)
	}
	lines[len(lines)-1] = benchChain(1, hops...)

	cases := []struct {
		name   string
		opts   []Option
		values []string
	}{
		{"hops_10", []Option{trusted}, []string{benchChain(8, hops...)}},
		{"hops_100", []Option{trusted}, []string{benchChain(98, hops...)}},
		{"hops_1000", []Option{trusted}, []string{benchChain(998, hops...)}},
		{"field_lines_20", []Option{trusted}, lines},
		{"proxy_count_1000", []Option{WithTrustedPeers(netip.MustParsePrefix("10.0.0.0/8")), WithTrustedProxiesCount(2)}, []string{benchChain(998, hops...)}},
		{"cache_disabled_1000", []Option{trusted, WithCacheDisabled()}, []string{benchChain(998, hops...)}},
	}
	for _, tc := range cases {
		ext, err := NewExtractor(tc.opts...)
		if err != nil {
			b.Fatal(err)
		}
		h := http.Header{HeaderXForwardedFor: tc.values}
		headers := httpx.NewHTTPHeaderGetter(&http.Request{Header: h})
		b.Run(tc.name, func(b *testing.B) {
			ctx := b.Context()
			if got := ext.Extract(ctx, peer, headers); got == peer {
				b.Fatalf("Extract() = peer %s, want the client entry", got)
			}
			b.ReportAllocs()
			for b.Loop() {
				ext.Extract(ctx, peer, headers)
			}
		})
	}
}
