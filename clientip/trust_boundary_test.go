// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package clientip_test

import (
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/clientip"
	"github.com/altessa-s/go-atlas-net/httpx"
)

func TestXFFTrustBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		hop  string
	}{
		{"private IPv4", "192.168.1.50"},
		{"private IPv6", "fd00::50"},
		{"mapped IPv4", "::ffff:192.168.1.50"},
		{"loopback", "127.0.0.1"},
		{"invalid", "garbage"},
		{"empty", ""},
		{"overlong", strings.Repeat("x", clientip.MaxIPLength+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			peer := netip.MustParseAddr("10.0.0.1")
			extractor, err := clientip.NewExtractor(clientip.WithTrustedProxies(netip.MustParsePrefix("10.0.0.1/32")))
			require.NoError(t, err)
			headers := httpx.NewHTTPHeaderGetter(&http.Request{Header: http.Header{
				"X-Forwarded-For": {"8.8.8.8, " + tc.hop + ", 10.0.0.1"},
				"X-Real-Ip":       {"1.1.1.1"},
			}})
			for range 2 { // Exercise cold and warm address caches.
				require.Equal(t, peer, extractor.Extract(t.Context(), peer, headers))
			}
		})
	}
}

// TestVendorHeaderSpoofing pins that a client behind a trusted proxy cannot
// pick its own address by sending a vendor header the proxy never sets.
func TestVendorHeaderSpoofing(t *testing.T) {
	t.Parallel()
	peer := netip.MustParseAddr("10.0.0.1")
	trusted := clientip.WithTrustedProxies(netip.MustParsePrefix("10.0.0.1/32"))
	for _, tc := range []struct {
		name    string
		opts    []clientip.Option
		headers http.Header
		want    netip.Addr
	}{
		{
			name:    "default ignores True-Client-IP",
			headers: http.Header{"True-Client-Ip": {"8.8.8.8"}},
			want:    peer,
		},
		{
			name:    "default ignores CF-Connecting-IP",
			headers: http.Header{"Cf-Connecting-Ip": {"8.8.8.8"}},
			want:    peer,
		},
		{
			name:    "default ignores X-Real-IP",
			headers: http.Header{"X-Real-Ip": {"8.8.8.8"}},
			want:    peer,
		},
		{
			name: "default uses X-Forwarded-For over spoofed vendor headers",
			headers: http.Header{
				"X-Forwarded-For": {"9.9.9.9"},
				"True-Client-Ip":  {"8.8.8.8"},
			},
			want: netip.MustParseAddr("9.9.9.9"),
		},
		{
			name: "private X-Real-IP does not fall through to True-Client-IP",
			opts: []clientip.Option{clientip.WithHeaders(clientip.HeaderXRealIP, clientip.HeaderTrueClientIP)},
			headers: http.Header{
				"X-Real-Ip":      {"192.168.1.5"},
				"True-Client-Ip": {"8.8.8.8"},
			},
			want: peer,
		},
		{
			name:    "configured X-Real-IP uses only its last value",
			opts:    []clientip.Option{clientip.WithHeaders(clientip.HeaderXRealIP)},
			headers: http.Header{"X-Real-Ip": {"8.8.8.8, 9.9.9.9"}},
			want:    netip.MustParseAddr("9.9.9.9"),
		},
		{
			name:    "configured X-Real-IP private last value is not skipped",
			opts:    []clientip.Option{clientip.WithHeaders(clientip.HeaderXRealIP)},
			headers: http.Header{"X-Real-Ip": {"8.8.8.8", "192.168.1.5"}},
			want:    peer,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			extractor, err := clientip.NewExtractor(append([]clientip.Option{trusted}, tc.opts...)...)
			require.NoError(t, err)
			for range 2 { // Exercise cold and warm address caches.
				require.Equal(t, tc.want, extractor.Extract(t.Context(), peer, httpx.NewHTTPHeaderGetter(&http.Request{Header: tc.headers})))
			}
		})
	}
}
