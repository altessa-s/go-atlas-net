// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/egressrule"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

func mustEgressPolicy(tb testing.TB, allow, deny []httpclient.EgressRule) *httpclient.EgressPolicy {
	tb.Helper()
	p, err := httpclient.NewEgressPolicy(allow, deny)
	require.NoError(tb, err)
	return p
}

func TestNewEgressPolicy_InvalidRules(t *testing.T) {
	t.Parallel()

	cidr := netip.MustParsePrefix("10.0.0.0/8")
	tests := []struct {
		name  string
		allow []httpclient.EgressRule
		deny  []httpclient.EgressRule
	}{
		{"deny without matchers", nil, []httpclient.EgressRule{{}}},
		{"allow without cidr", []httpclient.EgressRule{{Host: "*.corp"}}, nil},
		{"allow with port only", []httpclient.EgressRule{{Port: 443}}, nil},
		{"bad glob", nil, []httpclient.EgressRule{{Host: "[a-"}}},
		{"host with port", nil, []httpclient.EgressRule{{Host: "example.com:443"}}},
		{"host with scheme", nil, []httpclient.EgressRule{{Host: "http://example.com"}}},
		{"host with path", nil, []httpclient.EgressRule{{Host: "example.com/x"}}},
		{"host with userinfo", nil, []httpclient.EgressRule{{Host: "user@example.com"}}},
		{"host with space", nil, []httpclient.EgressRule{{Host: "example .com"}}},
		{"host with trailing dot", nil, []httpclient.EgressRule{{Host: "example.com."}}},
		{"invalid prefix", nil, []httpclient.EgressRule{{CIDR: netip.PrefixFrom(netip.MustParseAddr("10.0.0.0"), 33)}}},
		{"invalid allow prefix", []httpclient.EgressRule{{CIDR: netip.PrefixFrom(netip.MustParseAddr("::"), 129)}}, nil},
		{"second rule invalid", []httpclient.EgressRule{{CIDR: cidr}, {Host: "a"}}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p, err := httpclient.NewEgressPolicy(tc.allow, tc.deny)
			require.ErrorIs(t, err, httpclient.ErrInvalidEgressRule)
			require.Nil(t, p)
		})
	}
}

// TestParseEgressPolicy compiles textual rules as NewEgressPolicy compiles
// the parsed ones: the same decisions and labels, and the parse errors of the
// text form wrapping ErrInvalidEgressRule.
func TestParseEgressPolicy(t *testing.T) {
	t.Parallel()

	p, err := httpclient.ParseEgressPolicy(
		[]egressrule.Rule{{CIDR: "::ffff:10.1.0.0/112", Name: "corp"}},
		[]egressrule.Rule{{Host: "*.Internal", Port: 22}},
	)
	require.NoError(t, err)
	d := p.Explain("api.corp", netip.MustParseAddrPort("10.1.2.3:443"))
	require.True(t, d.Allowed)
	require.Equal(t, "corp cidr=10.1.0.0/16", d.Rule)
	d = p.Explain("db.internal", netip.MustParseAddrPort("10.1.2.3:22"))
	require.False(t, d.Allowed)
	require.Equal(t, "deny[0] host=*.internal port=22", d.Rule)

	for _, tc := range []struct {
		name        string
		allow, deny []egressrule.Rule
		want        string
	}{
		{"unparseable cidr", []egressrule.Rule{{CIDR: "10.0.0.0"}}, nil, "allow[0].cidr"},
		{"port out of range", nil, []egressrule.Rule{{Port: 65536}}, "deny[0].port"},
		{"engine rule", []egressrule.Rule{{Host: "a.example"}}, nil, "allow[0].cidr: required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, err := httpclient.ParseEgressPolicy(tc.allow, tc.deny)
			require.ErrorIs(t, err, httpclient.ErrInvalidEgressRule)
			require.ErrorContains(t, err, tc.want)
			require.Nil(t, p)
		})
	}
}

func TestEgressPolicy_Check(t *testing.T) {
	t.Parallel()

	p := mustEgressPolicy(t,
		[]httpclient.EgressRule{
			{CIDR: netip.MustParsePrefix("10.1.0.0/16")},
			{CIDR: netip.MustParsePrefix("10.2.0.0/16"), Host: "*.corp.example"},
			{CIDR: netip.MustParsePrefix("10.3.0.0/16"), Port: 8443},
			{CIDR: netip.MustParsePrefix("::ffff:192.168.7.0/120")},
			{CIDR: netip.MustParsePrefix("fd00:1::/32")},
			{CIDR: netip.MustParsePrefix("10.1.9.0/24")}, // overlapped by deny below
		},
		[]httpclient.EgressRule{
			{CIDR: netip.MustParsePrefix("203.0.113.0/24")},
			{CIDR: netip.MustParsePrefix("10.1.9.0/24")},
			{Host: "*.internal"},
			{Host: "Secret.Example.COM"},
			{Port: 25},
			{Host: "api.example.com", Port: 8080},
			{CIDR: netip.MustParsePrefix("2001:db8:dead::/48")},
		},
	)

	tests := []struct {
		name    string
		host    string
		addr    string
		blocked bool
		rule    bool // refused by a deny rule (SSRFError.Rule set)
	}{
		{"public v4", "example.com", "93.184.216.34:443", false, false},
		{"public v6", "example.com", "[2606:2800:220:1::1]:443", false, false},
		{"rfc1918 default", "example.com", "192.168.1.1:443", true, false},
		{"loopback default", "localhost", "127.0.0.1:80", true, false},
		{"loopback v6 default", "localhost", "[::1]:80", true, false},
		{"metadata", "metadata.example", "169.254.169.254:80", true, false},
		{"link-local v6 zoned", "example.com", "[fe80::1%eth0]:80", true, false},
		{"ula", "example.com", "[fd12::1]:80", true, false},
		{"cgnat", "example.com", "100.64.0.1:80", true, false},
		{"unspecified", "example.com", "0.0.0.0:80", true, false},
		{"mapped private", "example.com", "[::ffff:10.9.0.1]:80", true, false},
		{"mapped metadata", "example.com", "[::ffff:169.254.169.254]:80", true, false},
		{"mapped public", "example.com", "[::ffff:93.184.216.34]:80", false, false},
		{"allow cidr exempts private", "example.com", "10.1.2.3:443", false, false},
		{"allow cidr exempts mapped private", "example.com", "[::ffff:10.1.2.3]:443", false, false},
		{"allow cidr narrowed by host, match", "svc.corp.example", "10.2.0.1:443", false, false},
		{"allow cidr narrowed by host, case and dot", "SVC.Corp.Example.", "10.2.0.1:443", false, false},
		{"allow cidr narrowed by host, miss", "evil.example", "10.2.0.1:443", true, false},
		{"allow cidr narrowed by port, match", "x", "10.3.0.1:8443", false, false},
		{"allow cidr narrowed by port, miss", "x", "10.3.0.1:443", true, false},
		{"allow mapped prefix as v4", "x", "192.168.7.9:80", false, false},
		{"allow v6 prefix", "x", "[fd00:1::5]:80", false, false},
		{"deny public cidr", "example.com", "203.0.113.7:443", true, true},
		{"deny mapped address in v4 cidr", "example.com", "[::ffff:203.0.113.7]:443", true, true},
		{"deny wins over allow", "example.com", "10.1.9.1:443", true, true},
		{"deny host glob", "db.prod.internal", "93.184.216.34:443", true, true},
		{"deny host glob does not match apex", "internal", "93.184.216.34:443", false, false},
		{"deny host case insensitive", "secret.example.com", "93.184.216.34:443", true, true},
		{"deny host trailing dot", "SECRET.example.com.", "93.184.216.34:443", true, true},
		{"deny port", "mail.example.com", "93.184.216.34:25", true, true},
		{"deny host and port, both", "api.example.com", "93.184.216.34:8080", true, true},
		{"deny host and port, host only", "api.example.com", "93.184.216.34:443", false, false},
		{"deny v6 cidr", "example.com", "[2001:db8:dead::1]:443", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := p.Check(tc.host, netip.MustParseAddrPort(tc.addr))
			if !tc.blocked {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)
			ssrfErr := httpclient.IsSSRFError(err)
			require.NotNil(t, ssrfErr)
			require.Equal(t, tc.host, ssrfErr.Host)
			require.Equal(t, netip.MustParseAddrPort(tc.addr).Addr().WithZone("").Unmap().String(), ssrfErr.IP)
			if tc.rule {
				require.NotEmpty(t, ssrfErr.Rule)
				require.Contains(t, ssrfErr.Error(), ssrfErr.Rule)
			} else {
				require.Empty(t, ssrfErr.Rule)
			}
		})
	}

	t.Run("invalid address refused", func(t *testing.T) {
		t.Parallel()

		err := p.Check("example.com", netip.AddrPort{})
		require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)
	})
}

func TestEgressPolicy_NoRulesIsDefaultGuard(t *testing.T) {
	t.Parallel()

	p := mustEgressPolicy(t, nil, nil)
	for _, addr := range []string{"8.8.8.8:443", "10.0.0.1:80", "[::ffff:127.0.0.1]:80", "169.254.169.254:80", "[fd00::1]:80"} {
		want := httpclient.SSRFDialControl()("tcp", addr, nil)
		got := p.Check("h", netip.MustParseAddrPort(addr))
		require.Equal(t, want == nil, got == nil, addr)
	}
}

func TestNewEgressPolicy_CopiesRules(t *testing.T) {
	t.Parallel()

	allow := []httpclient.EgressRule{{CIDR: netip.MustParsePrefix("10.0.0.0/8")}}
	deny := []httpclient.EgressRule{{Host: "blocked.example"}}
	p := mustEgressPolicy(t, allow, deny)
	allow[0].CIDR = netip.MustParsePrefix("192.168.0.0/16")
	deny[0].Host = "other.example"

	require.NoError(t, p.Check("x", netip.MustParseAddrPort("10.0.0.1:80")))
	require.ErrorIs(t, p.Check("x", netip.MustParseAddrPort("192.168.0.1:80")), httpclient.ErrSSRFBlocked)
	require.ErrorIs(t, p.Check("blocked.example", netip.MustParseAddrPort("8.8.8.8:80")), httpclient.ErrSSRFBlocked)
	require.NoError(t, p.Check("other.example", netip.MustParseAddrPort("8.8.8.8:80")))
}

var loopbackAllow = []httpclient.EgressRule{
	{CIDR: netip.MustParsePrefix("127.0.0.0/8")},
	{CIDR: netip.MustParsePrefix("::1/128")},
}

func TestEgressPolicy_DialContext(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	addr := ln.Addr().String()

	t.Run("allowed address is the dialed one", func(t *testing.T) {
		t.Parallel()

		var seen atomic.Value
		d := &net.Dialer{Control: func(_, address string, _ syscall.RawConn) error {
			seen.Store(address)
			return nil
		}}
		dial := mustEgressPolicy(t, loopbackAllow, nil).DialContext(d)
		conn, err := dial(t.Context(), "tcp", addr)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		require.Equal(t, addr, conn.RemoteAddr().String())
		require.Equal(t, addr, seen.Load(), "the caller's hook runs after the policy, on the same address")
		require.Nil(t, d.ControlContext, "the caller's dialer is not modified")
	})

	t.Run("default denial", func(t *testing.T) {
		t.Parallel()

		var hook atomic.Int32
		d := &net.Dialer{ControlContext: func(context.Context, string, string, syscall.RawConn) error {
			hook.Add(1)
			return nil
		}}
		_, err := mustEgressPolicy(t, nil, nil).DialContext(d)(t.Context(), "tcp", addr)
		require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)
		require.Zero(t, hook.Load(), "a refusal stops before the caller's hook")
	})

	t.Run("deny port", func(t *testing.T) {
		t.Parallel()

		_, port, err := net.SplitHostPort(addr)
		require.NoError(t, err)
		n, err := strconv.ParseUint(port, 10, 16)
		require.NoError(t, err)
		p := mustEgressPolicy(t, loopbackAllow, []httpclient.EgressRule{{Port: uint16(n)}})
		_, err = p.DialContext(&net.Dialer{})(t.Context(), "tcp", addr)
		require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)
		require.NotEmpty(t, httpclient.IsSSRFError(err).Rule)
	})

	t.Run("deny host by name", func(t *testing.T) {
		t.Parallel()

		_, port, err := net.SplitHostPort(addr)
		require.NoError(t, err)
		p := mustEgressPolicy(t, loopbackAllow, []httpclient.EgressRule{{Host: "localhost"}})
		_, err = p.DialContext(&net.Dialer{})(t.Context(), "tcp", net.JoinHostPort("localhost", port))
		require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)
		require.Equal(t, "localhost", httpclient.IsSSRFError(err).Host)
	})

	t.Run("not host:port", func(t *testing.T) {
		t.Parallel()

		_, err := mustEgressPolicy(t, nil, nil).DialContext(&net.Dialer{})(t.Context(), "tcp", "no-port")
		require.Error(t, err)
	})
}

func TestWithEgressPolicy_Client(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, r.URL.Query().Get("to"), http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	srvURL, err := url.Parse(srv.URL)
	require.NoError(t, err)
	port, err := strconv.ParseUint(srvURL.Port(), 10, 16)
	require.NoError(t, err)

	t.Run("allow rule exempts loopback", func(t *testing.T) {
		t.Parallel()

		c := httpclient.New(httpclient.WithEgressPolicy(mustEgressPolicy(t, loopbackAllow, nil)), httpclient.WithRetryMax(0))
		resp, err := get(t, c, srv.URL)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
	})

	t.Run("deny wins over allow", func(t *testing.T) {
		t.Parallel()

		p := mustEgressPolicy(t, loopbackAllow, []httpclient.EgressRule{{CIDR: netip.MustParsePrefix("127.0.0.1/32"), Port: uint16(port)}})
		c := httpclient.New(httpclient.WithEgressPolicy(p), httpclient.WithRetryMax(0))
		_, err := get(t, c, srv.URL)
		require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)
		require.NotNil(t, httpclient.IsSSRFError(err))
	})

	t.Run("allowed CIDRs still apply", func(t *testing.T) {
		t.Parallel()

		p := mustEgressPolicy(t, nil, []httpclient.EgressRule{{Host: "*.internal"}})
		c := httpclient.New(httpclient.WithEgressPolicy(p), proxyLoopback, httpclient.WithRetryMax(0))
		resp, err := get(t, c, srv.URL)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
	})

	t.Run("no rules keeps the default denial", func(t *testing.T) {
		t.Parallel()

		c := httpclient.New(httpclient.WithEgressPolicy(mustEgressPolicy(t, nil, nil)), httpclient.WithRetryMax(0))
		_, err := get(t, c, srv.URL)
		require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)
	})

	t.Run("redirect target is checked", func(t *testing.T) {
		t.Parallel()

		var finals atomic.Int32
		final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			finals.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(final.Close)
		finalURL, err := url.Parse(final.URL)
		require.NoError(t, err)

		p := mustEgressPolicy(t, loopbackAllow, []httpclient.EgressRule{{Host: "localhost"}})
		c := httpclient.New(httpclient.WithEgressPolicy(p), httpclient.WithRetryMax(0))
		target := "http://" + net.JoinHostPort("localhost", finalURL.Port()) + "/final"
		_, err = get(t, c, srv.URL+"/redirect?to="+url.QueryEscape(target))
		require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)
		require.Zero(t, finals.Load(), "the refused redirect target is never reached")
	})

	t.Run("without SSRF protection the policy is off", func(t *testing.T) {
		t.Parallel()

		p := mustEgressPolicy(t, nil, []httpclient.EgressRule{{Port: uint16(port)}})
		c := httpclient.New(httpclient.WithEgressPolicy(p), httpclient.WithoutSSRFProtection(), httpclient.WithRetryMax(0))
		resp, err := get(t, c, srv.URL)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
	})
}

func TestWithEgressPolicy_Proxy(t *testing.T) {
	t.Parallel()

	p, proxyURL := startProxy(t)
	policy := mustEgressPolicy(t, loopbackAllow, []httpclient.EgressRule{
		{Host: "*.internal"},
		{Host: "mail.example", Port: 25},
		{CIDR: netip.MustParsePrefix("93.184.216.0/24")},
	})
	c := httpclient.New(httpclient.WithProxyURL(proxyURL), httpclient.WithEgressPolicy(policy), httpclient.WithRetryMax(0))

	for _, target := range []string{
		"http://db.internal/",                     // host deny rule, checked before the proxy
		"http://mail.example:25/",                 // host and port deny rule
		"http://93.184.216.34/",                   // literal IP in a deny CIDR
		"http://169.254.169.254/latest/meta-data", // literal private IP
		"http://[::ffff:10.0.0.1]/",
	} {
		_, err := get(t, c, target)
		require.ErrorIs(t, err, httpclient.ErrSSRFBlocked, target)
	}
	require.Empty(t, p.seen(), "a refused target never reaches the proxy")

	// A host name outside the deny rules is resolved by the proxy; the
	// connection to the proxy itself passes the policy (loopback allowed).
	resp, err := get(t, c, "http://mail.example/inbox")
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.Equal(t, []string{"http://mail.example/inbox"}, p.seen())
}

func TestSSRFError_Message(t *testing.T) {
	t.Parallel()

	plain := &httpclient.SSRFError{Host: "h", IP: "10.0.0.1"}
	require.NotContains(t, plain.Error(), "rule")
	ruled := &httpclient.SSRFError{Host: "h", IP: "8.8.8.8", Rule: "deny[0] port=25"}
	require.Contains(t, ruled.Error(), "deny[0] port=25")
	require.ErrorIs(t, ruled, httpclient.ErrSSRFBlocked)
}
