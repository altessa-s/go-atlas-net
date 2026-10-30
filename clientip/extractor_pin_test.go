// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package clientip

import (
	"bytes"
	"log/slog"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseXForwardedForHeader_Pin(t *testing.T) {
	t.Parallel()

	newDebug := func(t *testing.T, opt ...Option) (*Extractor, *bytes.Buffer) {
		t.Helper()
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		e, err := NewExtractor(append(opt, WithLogger(logger))...)
		require.NoError(t, err)
		return e, &buf
	}

	t.Run("no entries", func(t *testing.T) {
		t.Parallel()
		e, _ := newDebug(t)
		require.False(t, e.parseXForwardedForHeader(t.Context(), nil).IsValid())
	})

	t.Run("trusted proxy count is logged", func(t *testing.T) {
		t.Parallel()
		e, buf := newDebug(t, WithTrustedProxiesCount(1))
		got := e.parseXForwardedForHeader(t.Context(), []string{"8.8.8.8", "10.0.0.1"})
		require.Equal(t, netip.MustParseAddr("8.8.8.8"), got)
		require.Contains(t, buf.String(), "parsing X-Forwarded-For")
		require.Contains(t, buf.String(), "checking IP by trusted proxy count")
		require.Contains(t, buf.String(), "found IP by trusted proxy count")
	})

	t.Run("an over-long entry ends the walk", func(t *testing.T) {
		t.Parallel()
		e, _ := newDebug(t)
		got := e.parseXForwardedForHeader(t.Context(), []string{"8.8.8.8", strings.Repeat("1", MaxIPLength+1)})
		require.False(t, got.IsValid())
	})
}

// TestExtractor_HeaderEntriesNotInterned pins that client-written header
// entries are parsed in place: a stream of distinct values must not allocate
// per entry (the process-wide interner used to copy every one of them).
func TestExtractor_HeaderEntriesNotInterned(t *testing.T) {
	// Serial: testing.AllocsPerRun refuses to run in a parallel test.

	const n = 1024
	peer := netip.MustParseAddr("10.0.0.1")
	chains := make([]*benchHeaders, n)
	single := make([]*benchHeaders, n)
	for i := range n {
		client := netip.AddrFrom4([4]byte{198, 51, byte(i >> 8), byte(i)}).String()
		chains[i] = &benchHeaders{data: map[string][]string{HeaderXForwardedFor: {client + ", 10.1.0.1, 10.2.0.1"}}}
		single[i] = &benchHeaders{data: map[string][]string{HeaderXRealIP: {client}}}
	}

	tests := []struct {
		name    string
		headers []*benchHeaders
		opt     []Option
	}{
		{name: "x-forwarded-for", headers: chains, opt: []Option{
			WithTrustedProxies(netip.MustParsePrefix("10.0.0.0/8")),
		}},
		{name: "x-real-ip", headers: single, opt: []Option{
			WithTrustedPeers(netip.MustParsePrefix("10.0.0.0/8")), WithHeaders(HeaderXRealIP),
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, err := NewExtractor(append(tc.opt, WithCacheDisabled())...)
			require.NoError(t, err)

			i := 0
			allocs := testing.AllocsPerRun(n-1, func() {
				got := e.Extract(t.Context(), peer, tc.headers[i])
				i++
				if got == peer {
					t.Fatal("header entry was not used")
				}
			})
			// At most the extractor's entry slice; nothing per entry.
			require.LessOrEqual(t, allocs, 1.0)
		})
	}
}
