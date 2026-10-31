// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package clientip_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/clientip"
	"github.com/altessa-s/go-atlas-net/httpx"
)

func headerGetter(kv ...string) clientip.HeaderGetter {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return httpx.NewHTTPHeaderGetter(&http.Request{Header: h})
}

// TestExtract_DebugLoggingKeepsResult runs every extraction path with a debug
// logger and with the default discarding one: logging must never change the
// result, and each path must leave a debug record.
func TestExtract_DebugLoggingKeepsResult(t *testing.T) {
	t.Parallel()

	peer := netip.MustParseAddr("10.0.0.1")
	cases := []struct {
		name    string
		peer    netip.Addr
		opts    []clientip.Option
		headers clientip.HeaderGetter
		want    string
		logged  string
	}{
		{
			name:    "untrusted_peer",
			peer:    netip.MustParseAddr("198.51.100.9"),
			headers: headerGetter(clientip.HeaderXForwardedFor, "8.8.8.8"),
			want:    "198.51.100.9",
			logged:  "using peer IP as real IP",
		},
		{
			name:    "forwarded_for_client",
			opts:    []clientip.Option{clientip.WithTrustedPeers(netip.MustParsePrefix("10.0.0.0/8"))},
			headers: headerGetter(clientip.HeaderXForwardedFor, "8.8.8.8"),
			want:    "8.8.8.8",
			logged:  "found IP from X-Forwarded-For",
		},
		{
			name: "proxy_count",
			opts: []clientip.Option{
				clientip.WithTrustedPeers(netip.MustParsePrefix("10.0.0.0/8")),
				clientip.WithTrustedProxiesCount(1),
			},
			headers: headerGetter(clientip.HeaderXForwardedFor, "8.8.8.8, 9.9.9.9"),
			want:    "8.8.8.8",
			logged:  "found IP by trusted proxy count",
		},
		{
			name: "single_value_header_public",
			opts: []clientip.Option{
				clientip.WithTrustedPeers(netip.MustParsePrefix("10.0.0.0/8")),
				clientip.WithHeaders(clientip.HeaderXRealIP),
			},
			headers: headerGetter(clientip.HeaderXRealIP, "1.1.1.1"),
			want:    "1.1.1.1",
			logged:  "found public IP",
		},
		{
			name: "single_value_header_malformed",
			opts: []clientip.Option{
				clientip.WithTrustedPeers(netip.MustParsePrefix("10.0.0.0/8")),
				clientip.WithHeaders(clientip.HeaderXRealIP),
			},
			headers: headerGetter(clientip.HeaderXRealIP, "not-an-ip"),
			want:    "10.0.0.1",
			logged:  "failed to parse IP",
		},
		{
			name:    "no_header",
			opts:    []clientip.Option{clientip.WithTrustedPeers(netip.MustParsePrefix("10.0.0.0/8"))},
			headers: headerGetter(),
			want:    "10.0.0.1",
			logged:  "no IP from headers, falling back to peer IP",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := tc.peer
			if !p.IsValid() {
				p = peer
			}

			plain, err := clientip.NewExtractor(tc.opts...)
			require.NoError(t, err)
			require.Equal(t, tc.want, plain.Extract(t.Context(), p, tc.headers).String())

			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			logged, err := clientip.NewExtractor(append(tc.opts, clientip.WithLogger(logger))...)
			require.NoError(t, err)
			require.Equal(t, tc.want, logged.Extract(t.Context(), p, tc.headers).String())
			require.Contains(t, buf.String(), tc.logged)
		})
	}
}

// TestExtract_MalformedHeaderNotLoggedAboveDebug: header values come from
// clients, so a malformed one must not reach a logger above debug level.
func TestExtract_MalformedHeaderNotLoggedAboveDebug(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	e, err := clientip.NewExtractor(
		clientip.WithTrustedPeers(netip.MustParsePrefix("10.0.0.0/8")),
		clientip.WithHeaders(clientip.HeaderXRealIP, clientip.HeaderXForwardedFor),
		clientip.WithLogger(logger),
	)
	require.NoError(t, err)

	got := e.Extract(t.Context(), netip.MustParseAddr("10.0.0.1"), headerGetter(clientip.HeaderXRealIP, "<script>"))
	require.Equal(t, "10.0.0.1", got.String())
	require.Empty(t, buf.String())
}

// TestExtract_SingleValueHeaderCache repeats single-value header lookups so
// that the second one is served from the address cache: a cached public
// address is returned, a cached private one still falls back to the peer.
func TestExtract_SingleValueHeaderCache(t *testing.T) {
	t.Parallel()

	peer := netip.MustParseAddr("10.0.0.1")
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"public", "1.1.1.1", "1.1.1.1"},
		{"private", "192.168.1.5", "10.0.0.1"},
		{"mapped_private", "::ffff:192.168.1.5", "10.0.0.1"},
		{"mapped_public", "::ffff:1.1.1.1", "1.1.1.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			for _, opts := range [][]clientip.Option{{clientip.WithCacheSize(4)}, {clientip.WithCacheDisabled()}} {
				e, err := clientip.NewExtractor(append(opts,
					clientip.WithTrustedPeers(netip.MustParsePrefix("10.0.0.0/8")),
					clientip.WithHeaders(clientip.HeaderCFConnectingIP),
				)...)
				require.NoError(t, err)
				for range 3 {
					got := e.Extract(t.Context(), peer, headerGetter(clientip.HeaderCFConnectingIP, tc.value))
					require.Equal(t, tc.want, got.String())
				}
			}
		})
	}
}

// TestExtract_SingleValueHeaderOverlong: an overlong entry of a single-value
// header still decides (the peer is used) instead of falling through to the
// next configured header the same client may have forged.
func TestExtract_SingleValueHeaderOverlong(t *testing.T) {
	t.Parallel()

	e, err := clientip.NewExtractor(
		clientip.WithTrustedPeers(netip.MustParsePrefix("10.0.0.0/8")),
		clientip.WithHeaders(clientip.HeaderTrueClientIP, clientip.HeaderXForwardedFor),
	)
	require.NoError(t, err)

	headers := headerGetter(
		clientip.HeaderTrueClientIP, strings.Repeat("1", clientip.MaxIPLength+1),
		clientip.HeaderXForwardedFor, "8.8.8.8",
	)
	require.Equal(t, "10.0.0.1", e.Extract(t.Context(), netip.MustParseAddr("10.0.0.1"), headers).String())
}

// TestWithTrustedPeers_EmptyCallKeepsPrefixes pins that an empty call does not
// clear previously configured prefixes, so passing an empty configuration
// value through cannot silently distrust every proxy.
func TestWithTrustedPeers_EmptyCallKeepsPrefixes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opts []clientip.Option
	}{
		{"peers", []clientip.Option{clientip.WithTrustedPeers(netip.MustParsePrefix("10.0.0.0/8")), clientip.WithTrustedPeers()}},
		{"proxies", []clientip.Option{clientip.WithTrustedProxies(netip.MustParsePrefix("10.0.0.0/8")), clientip.WithTrustedProxies()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e, err := clientip.NewExtractor(tc.opts...)
			require.NoError(t, err)
			got := e.Extract(t.Context(), netip.MustParseAddr("10.0.0.1"), headerGetter(clientip.HeaderXForwardedFor, "8.8.8.8"))
			require.Equal(t, "8.8.8.8", got.String())
		})
	}
}
