// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAllowPrivateAllowedCIDRs(t *testing.T) {
	t.Parallel()

	base, err := NewEgressPolicy(nil, []EgressRule{{Host: "blocked"}})
	require.NoError(t, err)

	p := base.AllowPrivateDestinations()
	extended := p.withAllowedCIDRs([]netip.Prefix{{}, netip.MustParsePrefix("::ffff:10.0.0.0/104")})
	dst := netip.MustParseAddrPort("10.1.2.3:80")
	require.Equal(t, EgressAllowPrivate, p.Explain("h", dst).Reason)
	require.Equal(t, EgressPrivateDefault, base.Explain("h", dst).Reason)
	d := extended.Explain("h", dst)
	require.Equal(t, EgressAllowRule, d.Reason)
	require.Equal(t, "allowedCIDRs[1] cidr=10.0.0.0/8", d.Rule)
	require.Equal(t, EgressDenyRule, extended.Explain("blocked", dst).Reason)
	require.Equal(t, EgressAllowPrivate, extended.Explain("h", netip.MustParseAddrPort("192.168.1.1:80")).Reason)
	require.Same(t, p, p.withAllowedCIDRs(nil))
}

func TestAllowPrivateKeepsTransportValidation(t *testing.T) {
	t.Parallel()

	p := (&EgressPolicy{}).AllowPrivateDestinations()
	opts := newOptions(WithEgressPolicy(p), WithTransport(&http.Transport{
		DialTLSContext: func(context.Context, string, string) (net.Conn, error) { panic("must not dial") },
	}))
	require.ErrorIs(t, validateSSRFTransport(opts), ErrSSRFUnsafeTransport)
}
