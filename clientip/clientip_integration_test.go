//go:build integration

// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package clientip_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/clientip"
	"github.com/altessa-s/go-atlas-net/httpx"
)

// clientIPMiddleware resolves the client IP of every request with e and
// stores it in the request context, as a service would.
func clientIPMiddleware(e *clientip.Extractor, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		ip := e.Extract(r.Context(), peer.Addr(), httpx.NewHTTPHeaderGetter(r))
		next.ServeHTTP(w, r.WithContext(clientip.NewContext(r.Context(), ip)))
	})
}

// startClientIPBackend serves the client IP found in the request context.
func startClientIPBackend(tb testing.TB, opts ...clientip.Option) *httptest.Server {
	tb.Helper()

	e, err := clientip.NewExtractor(opts...)
	require.NoError(tb, err)
	srv := httptest.NewServer(clientIPMiddleware(e, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, clientip.FromContext(r.Context()).String())
	})))
	tb.Cleanup(srv.Close)
	return srv
}

// startForwardingHop starts a real reverse proxy in front of target. It
// appends its peer address to X-Forwarded-For, as load balancers do.
func startForwardingHop(tb testing.TB, target string) *httptest.Server {
	tb.Helper()

	u, err := url.Parse(target)
	require.NoError(tb, err)
	srv := httptest.NewServer(httputil.NewSingleHostReverseProxy(u))
	tb.Cleanup(srv.Close)
	return srv
}

// fetchClientIP sends a GET with the given header lines and returns the body.
func fetchClientIP(tb testing.TB, rawURL string, header http.Header) string {
	tb.Helper()

	req, err := http.NewRequestWithContext(tb.Context(), http.MethodGet, rawURL, nil)
	require.NoError(tb, err)
	req.Header = header
	resp, err := http.DefaultClient.Do(req)
	require.NoError(tb, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(tb, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(tb, err)
	return string(body)
}

func longChain(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "1.0.0." + strconv.Itoa(i%250+1)
	}
	return strings.Join(parts, ", ")
}

// TestIntegration_ClientIPOverRealHTTP extracts client IPs from requests sent
// over loopback, directly and through two real reverse proxy hops that append
// their peers to X-Forwarded-For.
func TestIntegration_ClientIPOverRealHTTP(t *testing.T) {
	t.Parallel()

	loopback := netip.MustParsePrefix("127.0.0.0/8")
	cases := []struct {
		name   string
		opts   []clientip.Option
		hops   int
		header http.Header
		want   string
	}{
		{
			name:   "untrusted_peer_ignores_spoofed_header",
			opts:   []clientip.Option{clientip.WithTrustedPeers(netip.MustParsePrefix("10.0.0.0/8"))},
			header: http.Header{"X-Forwarded-For": {"8.8.8.8"}},
			want:   "127.0.0.1",
		},
		{
			name:   "trusted_peer_without_header",
			opts:   []clientip.Option{clientip.WithTrustedPeers(loopback)},
			header: http.Header{},
			want:   "127.0.0.1",
		},
		{
			name:   "trusted_peer_forwards_client",
			opts:   []clientip.Option{clientip.WithTrustedPeers(loopback)},
			header: http.Header{"X-Forwarded-For": {"8.8.8.8"}},
			want:   "8.8.8.8",
		},
		{
			name:   "multiple_header_lines_rightmost_wins",
			opts:   []clientip.Option{clientip.WithTrustedPeers(loopback)},
			header: http.Header{"X-Forwarded-For": {"6.6.6.6, 7.7.7.7", "8.8.8.8"}},
			want:   "8.8.8.8",
		},
		{
			name:   "private_entry_falls_back_to_peer",
			opts:   []clientip.Option{clientip.WithTrustedPeers(loopback)},
			header: http.Header{"X-Forwarded-For": {"8.8.8.8, 192.168.1.10"}},
			want:   "127.0.0.1",
		},
		{
			name:   "two_hops_skip_trusted_proxies",
			opts:   []clientip.Option{clientip.WithTrustedProxies(loopback)},
			hops:   2,
			header: http.Header{"X-Forwarded-For": {"6.6.6.6, 8.8.8.8"}},
			want:   "8.8.8.8",
		},
		{
			name:   "two_hops_proxy_count",
			opts:   []clientip.Option{clientip.WithTrustPrivatePeers(), clientip.WithTrustedProxiesCount(2)},
			hops:   2,
			header: http.Header{"X-Forwarded-For": {"6.6.6.6, 8.8.8.8"}},
			want:   "8.8.8.8",
		},
		{
			name:   "two_hops_long_padded_chain",
			opts:   []clientip.Option{clientip.WithTrustedProxies(loopback)},
			hops:   2,
			header: http.Header{"X-Forwarded-For": {longChain(200) + ", 8.8.8.8"}},
			want:   "8.8.8.8",
		},
		{
			name:   "two_hops_only_proxies_falls_back_to_peer",
			opts:   []clientip.Option{clientip.WithTrustedProxies(loopback)},
			hops:   2,
			header: http.Header{},
			want:   "127.0.0.1",
		},
		{
			name: "vendor_header_first",
			opts: []clientip.Option{
				clientip.WithTrustedPeers(loopback),
				clientip.WithHeaders(clientip.HeaderCFConnectingIP, clientip.HeaderXForwardedFor),
			},
			header: http.Header{"Cf-Connecting-Ip": {"9.9.9.9"}, "X-Forwarded-For": {"8.8.8.8"}},
			want:   "9.9.9.9",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entry := startClientIPBackend(t, tc.opts...).URL
			for range tc.hops {
				entry = startForwardingHop(t, entry).URL
			}
			require.Equal(t, tc.want, fetchClientIP(t, entry, tc.header))
		})
	}
}
