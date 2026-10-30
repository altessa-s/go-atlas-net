// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package client_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

// Review finding R18-1: a host rule written with Unicode labels was only lower-cased, while net/http dials an internationalized
// name in its ASCII (punycode) form, so a deny rule for bücher.example never matched the name a direct dial checks, and matched
// the URL host a proxy check sees: the same rule refused one path and allowed the other. Rules and hosts are both matched in the
// ASCII form now.

var publicAddr = netip.MustParseAddrPort("93.184.216.34:80")

// TestEgressPolicy_InternationalizedHosts checks Unicode and punycode rules against Unicode and punycode host names.
func TestEgressPolicy_InternationalizedHosts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, rule, host string
		refused          bool
	}{
		{"unicode rule, dialed name", "bücher.example", "xn--bcher-kva.example", true},
		{"unicode rule, url host", "bücher.example", "bücher.example", true},
		{"unicode rule, upper-case url host", "bücher.example", "BÜCHER.example.", true},
		{"upper-case unicode rule", "BÜCHER.EXAMPLE", "xn--bcher-kva.example", true},
		{"punycode rule, url host", "xn--bcher-kva.example", "bücher.example", true},
		{"glob over unicode label", "*.bücher.example", "a.xn--bcher-kva.example", true},
		{"other name", "bücher.example", "xn--bchr-kva.example", false},
		{"ascii neighbour", "bücher.example", "bucher.example", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := mustEgressPolicy(t, nil, []httpclient.EgressRule{{Host: tt.rule}})
			err := p.Check(tt.host, publicAddr)
			if !tt.refused {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)
			require.Contains(t, httpclient.IsSSRFError(err).Rule, "deny[0]")
		})
	}
}

// TestNewEgressPolicy_UnicodeGlobLabel rejects a Unicode label holding a glob metacharacter, which has no punycode form.
func TestNewEgressPolicy_UnicodeGlobLabel(t *testing.T) {
	t.Parallel()

	for _, host := range []string{"bü*.example", "b?ücher.example", "[ü]x.example"} {
		_, err := httpclient.NewEgressPolicy(nil, []httpclient.EgressRule{{Host: host}})
		require.ErrorIs(t, err, httpclient.ErrInvalidEgressRule, host)
	}
}

// TestEgressPolicy_DialedNameMatchesUnicodeRule checks the name net/http hands the dialer for a Unicode URL against a Unicode
// deny rule: the punycode form, which the rule matches.
func TestEgressPolicy_DialedNameMatchesUnicodeRule(t *testing.T) {
	t.Parallel()

	errDialed := errors.New("dialed")
	var dialed string
	transport := &http.Transport{DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = addr
		return nil, errDialed
	}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://bücher.example/", http.NoBody)
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	if resp != nil {
		httpclient.CloseBody(resp.Body)
	}
	require.ErrorIs(t, err, errDialed)

	host, _, err := net.SplitHostPort(dialed)
	require.NoError(t, err)
	require.Equal(t, "xn--bcher-kva.example", host)
	p := mustEgressPolicy(t, nil, []httpclient.EgressRule{{Host: "bücher.example"}})
	require.ErrorIs(t, p.Check(host, publicAddr), httpclient.ErrSSRFBlocked)
}

// TestWithEgressPolicy_ProxyUnicodeHosts refuses Unicode URL hosts before the proxy, whether the rule is written in Unicode or
// in punycode.
func TestWithEgressPolicy_ProxyUnicodeHosts(t *testing.T) {
	t.Parallel()

	p, proxyURL := startProxy(t)
	policy := mustEgressPolicy(t, loopbackAllow, []httpclient.EgressRule{
		{Host: "bücher.example"},
		{Host: "xn--caf-dma.example"},
	})
	c := httpclient.New(httpclient.WithProxyURL(proxyURL), httpclient.WithEgressPolicy(policy), httpclient.WithRetryMax(0))
	for _, target := range []string{"http://bücher.example/", "http://xn--bcher-kva.example/", "http://café.example/"} {
		_, err := get(t, c, target)
		require.ErrorIs(t, err, httpclient.ErrSSRFBlocked, target)
	}
	require.Empty(t, p.seen(), "a refused target never reaches the proxy")
}

// Review finding R19-1: IDNA reads the ideographic (U+3002), fullwidth (U+FF0E) and halfwidth ideographic (U+FF61) full stops
// as label separators. A host ending in one became "blocked.example." only after the trailing dot was trimmed, so it missed the
// deny rule for blocked.example on the proxy path, while net/http sent the proxy that very name; a rule spelled with one kept an
// ineffective trailing-dot pattern. The trailing dot is trimmed after the ASCII conversion, rules map these stops to dots, and a
// rule ending in one is rejected.

var unicodeStops = []string{"。", "．", "｡"}

// TestEgressPolicy_UnicodeFullStops checks hosts and rules spelled with Unicode full stops, inside and at the end of the name.
func TestEgressPolicy_UnicodeFullStops(t *testing.T) {
	t.Parallel()

	ascii := mustEgressPolicy(t, nil, []httpclient.EgressRule{{Host: "blocked.example"}})
	for _, stop := range unicodeStops {
		for _, host := range []string{"blocked.example" + stop, "blocked" + stop + "example", "BLOCKED" + stop + "example" + stop} {
			require.ErrorIs(t, ascii.Check(host, publicAddr), httpclient.ErrSSRFBlocked, "%+q", host)
		}

		spelled := mustEgressPolicy(t, nil, []httpclient.EgressRule{{Host: "blocked" + stop + "example"}, {Host: "*" + stop + "bücher" + stop + "example"}})
		for _, host := range []string{"blocked.example", "a.xn--bcher-kva.example"} {
			require.ErrorIs(t, spelled.Check(host, publicAddr), httpclient.ErrSSRFBlocked, "%+q", host)
		}
		require.NoError(t, spelled.Check("blocked.example.org", publicAddr))

		for _, host := range []string{"blocked.example" + stop, "bücher" + stop} {
			_, err := httpclient.NewEgressPolicy(nil, []httpclient.EgressRule{{Host: host}})
			require.ErrorIs(t, err, httpclient.ErrInvalidEgressRule, "%+q", host)
		}
	}
}

// TestWithEgressPolicy_ProxyUnicodeFullStops refuses, before the proxy, URL hosts whose Unicode full stops net/http turns into
// a denied name or a literal private IP, for plain requests and CONNECT tunnels.
func TestWithEgressPolicy_ProxyUnicodeFullStops(t *testing.T) {
	t.Parallel()

	p, proxyURL := startProxy(t)
	policy := mustEgressPolicy(t, loopbackAllow, []httpclient.EgressRule{{Host: "blocked.example"}})
	c := httpclient.New(httpclient.WithProxyURL(proxyURL), httpclient.WithEgressPolicy(policy), httpclient.WithRetryMax(0))
	for _, stop := range unicodeStops {
		for _, target := range []string{
			"http://blocked.example" + stop + "/",
			"https://blocked.example" + stop + "/",
			"https://blocked" + stop + "example/",
			"http://169" + stop + "254" + stop + "169" + stop + "254/",
		} {
			_, err := get(t, c, target)
			require.ErrorIs(t, err, httpclient.ErrSSRFBlocked, "%+q", target)
		}
	}
	require.Empty(t, p.seen(), "a refused target never reaches the proxy")
}

// TestSSRFProxy_UnicodeFullStops: review finding R20-1 — without an egress policy, the built-in SSRF guard of a proxied client
// parsed the URL host as it was written, so a literal private IP spelled with Unicode full stops passed it, while net/http sent the
// proxy the ASCII literal IP. The host is read in that ASCII form now, for plain requests and CONNECT tunnels.
func TestSSRFProxy_UnicodeFullStops(t *testing.T) {
	t.Parallel()

	p, proxyURL := startProxy(t)
	c := httpclient.New(httpclient.WithProxyURL(proxyURL), proxyLoopback, httpclient.WithRetryMax(0))
	for _, stop := range unicodeStops {
		for _, target := range []string{
			"http://169" + stop + "254" + stop + "169" + stop + "254/",
			"https://169" + stop + "254" + stop + "169" + stop + "254/",
			"http://10" + stop + "0" + stop + "0" + stop + "1" + stop + "/",
		} {
			_, err := get(t, c, target)
			require.ErrorIs(t, err, httpclient.ErrSSRFBlocked, "%+q", target)
		}
	}
	for _, target := range []string{"http://１６９．２５４．１６９．２５４/", "https://１０．０．０．１/"} {
		_, err := get(t, c, target)
		require.ErrorIs(t, err, httpclient.ErrSSRFBlocked, "%+q", target)
	}
	require.Empty(t, p.seen(), "a refused target never reaches the proxy")
}
