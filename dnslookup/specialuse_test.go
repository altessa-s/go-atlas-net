// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package dnslookup_test

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/dnslookup"
)

func TestLookupMX_SpecialUseSnapshot(t *testing.T) {
	t.Parallel()

	// IANA Special-Use Domain Names registry, retrieved 2026-11-28 and last updated 2026-05-22.
	// https://www.iana.org/assignments/special-use-domain-names
	for _, domain := range [...]string{
		"alt.",
		"6tisch.arpa.",
		"eap.arpa.",
		"eap-noob.arpa.",
		"home.arpa.",
		"10.in-addr.arpa.",
		"254.169.in-addr.arpa.",
		"16.172.in-addr.arpa.",
		"17.172.in-addr.arpa.",
		"18.172.in-addr.arpa.",
		"19.172.in-addr.arpa.",
		"20.172.in-addr.arpa.",
		"21.172.in-addr.arpa.",
		"22.172.in-addr.arpa.",
		"23.172.in-addr.arpa.",
		"24.172.in-addr.arpa.",
		"25.172.in-addr.arpa.",
		"26.172.in-addr.arpa.",
		"27.172.in-addr.arpa.",
		"28.172.in-addr.arpa.",
		"29.172.in-addr.arpa.",
		"30.172.in-addr.arpa.",
		"31.172.in-addr.arpa.",
		"170.0.0.192.in-addr.arpa.",
		"171.0.0.192.in-addr.arpa.",
		"168.192.in-addr.arpa.",
		"8.e.f.ip6.arpa.",
		"9.e.f.ip6.arpa.",
		"a.e.f.ip6.arpa.",
		"b.e.f.ip6.arpa.",
		"ipv4only.arpa.",
		"resolver.arpa.",
		"service.arpa.",
		"example.",
		"example.com.",
		"example.net.",
		"example.org.",
		"invalid.",
		"local.",
		"localhost.",
		"onion.",
		"test.",
	} {
		t.Run(domain, func(t *testing.T) {
			t.Parallel()

			var calls int

			l := dnslookup.New(dnslookup.WithResolver(resolverFunc(func(context.Context, string) ([]*net.MX, error) {
				calls++
				return nil, nil
			})))
			for _, name := range []string{domain, "mx." + domain} {
				mx, err := l.LookupMX(t.Context(), name)
				require.ErrorIs(t, err, dnslookup.ErrRefused, "query %q", name)
				require.Empty(t, mx)
				require.Zero(t, calls, "query %q", name)
			}
		})
	}
}
