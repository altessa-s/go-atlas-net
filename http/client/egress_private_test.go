// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/egressrule"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

func TestEgressPolicyAllowPrivateDecisions(t *testing.T) {
	t.Parallel()

	base, err := httpclient.ParseEgressPolicy(
		[]egressrule.Rule{{Name: "explicit", CIDR: "10.0.0.0/8"}},
		[]egressrule.Rule{{Name: egressrule.Label(egressrule.Deny, 7), Host: "blocked"}},
	)
	require.NoError(t, err)

	p := base.AllowPrivateDestinations()

	require.Equal(t, "allow_private", httpclient.EgressAllowPrivate.String())

	for _, addr := range []string{
		"127.0.0.1:80", "192.168.0.1:80", "169.254.169.254:80", "0.0.0.0:80",
		"[::1]:80", "[fd00::1]:80", "[fe80::1%en0]:80", "[::ffff:127.0.0.1]:80", "[2001:db8::1]:80",
	} {
		dst := netip.MustParseAddrPort(addr)
		d := p.Explain("h", dst)
		require.True(t, d.Allowed, addr)
		require.Equal(t, httpclient.EgressAllowPrivate, d.Reason, addr)
		require.Empty(t, d.Rule)
		require.NoError(t, d.Err())
		require.NoError(t, p.Check("h", dst))
		require.NoError(t, p.CheckHost(dst.Addr().String(), 80))
		require.Equal(t, httpclient.EgressPrivateDefault, base.Explain("h", dst).Reason)
		require.Equal(t, d, p.AllowPrivateDestinations().Explain("h", dst))
	}

	for _, addr := range []string{"10.1.2.3:80", "8.8.8.8:80", "[2606:4700::1111]:80"} {
		dst := netip.MustParseAddrPort(addr)
		require.Equal(t, base.Explain("h", dst), p.Explain("h", dst))
		d := p.Explain("blocked", dst)
		require.Equal(t, httpclient.EgressDenyRule, d.Reason)
		require.Equal(t, "deny[7] host=blocked", d.Rule)
		require.ErrorIs(t, d.Err(), httpclient.ErrSSRFBlocked)
	}

	require.ErrorIs(t, p.CheckHost("blocked", 80), httpclient.ErrSSRFBlocked)
	require.NoError(t, p.CheckHost("127。0。0。1", 80))
	require.Equal(t, httpclient.EgressInvalidAddress, p.Explain("h", netip.AddrPort{}).Reason)
	require.ErrorIs(t, p.Check("h", netip.AddrPort{}), httpclient.ErrSSRFBlocked)
}

func TestEgressPolicyAllowPrivateDial(t *testing.T) {
	t.Parallel()

	stop := errors.New("stop before connect")
	calls := 0
	dialer := &net.Dialer{ControlContext: func(_ context.Context, _, addr string, _ syscall.RawConn) error {
		calls++

		require.Equal(t, "127.0.0.1:80", addr)

		return stop
	}}
	p := mustEgressPolicy(t, nil, nil).AllowPrivateDestinations()
	_, err := p.DialContext(dialer)(t.Context(), "tcp4", "localhost:80")
	require.ErrorIs(t, err, stop)
	require.Positive(t, calls)
	calls = 0

	p = mustEgressPolicy(t, nil, []httpclient.EgressRule{{CIDR: netip.MustParsePrefix("127.0.0.0/8")}}).AllowPrivateDestinations()
	_, err = p.DialContext(dialer)(t.Context(), "tcp4", "localhost:80")
	require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)
	require.Zero(t, calls)
}

func TestEgressPolicyAllowPrivateTransport(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	for _, blocked := range []bool{false, true} {
		var deny []httpclient.EgressRule
		if blocked {
			deny = []httpclient.EgressRule{{CIDR: netip.MustParsePrefix("127.0.0.0/8")}}
		}

		p := mustEgressPolicy(t, nil, deny).AllowPrivateDestinations()
		c := httpclient.New(httpclient.WithEgressPolicy(p), httpclient.WithRetryMax(0),
			httpclient.WithSSRFAllowedCIDRs(netip.MustParsePrefix("10.0.0.0/8")))

		_, err := get(t, c, srv.URL)
		if blocked {
			require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)
		} else {
			require.NoError(t, err)
		}
	}
}

func TestEgressPolicyAllowPrivateProxy(t *testing.T) {
	t.Parallel()

	proxy, proxyURL := startProxy(t)
	p := mustEgressPolicy(t, nil, []httpclient.EgressRule{
		{Host: "blocked"}, {CIDR: netip.MustParsePrefix("10.0.0.0/8")},
	}).AllowPrivateDestinations()
	c := httpclient.New(httpclient.WithProxyURL(proxyURL), httpclient.WithEgressPolicy(p), httpclient.WithRetryMax(0))

	for _, target := range []string{"http://blocked/", "http://10.1.2.3/"} {
		_, err := get(t, c, target)
		require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)
	}

	require.Empty(t, proxy.seen())
	_, err := get(t, c, "http://169.254.169.254/")
	require.NoError(t, err)
	require.Equal(t, []string{"http://169.254.169.254/"}, proxy.seen())
}
