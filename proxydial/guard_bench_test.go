// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package proxydial

import (
	"context"
	"net/netip"
	"testing"
)

// staticResolver answers every lookup with the same addresses.
type staticResolver []netip.Addr

func (r staticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return r, nil
}

func allowPublic(_ string, dst netip.AddrPort) error {
	if dst.Addr().IsPrivate() || dst.Addr().IsLoopback() {
		return context.Canceled
	}
	return nil
}

// BenchmarkApprovedTargets measures the per-dial work the SSRF guard adds
// before the proxy is contacted: lookup, normalization and one guard call
// per resolved address.
func BenchmarkApprovedTargets(b *testing.B) {
	cases := []struct {
		name     string
		addr     string
		resolver Resolver
	}{
		{"hostname_v4", "example.com:443", staticResolver{netip.MustParseAddr("93.184.216.34")}},
		{"hostname_dual", "example.com:443", staticResolver{
			netip.MustParseAddr("2606:2800:220:1::1"), netip.MustParseAddr("93.184.216.34"),
		}},
		{"literal", "93.184.216.34:443", staticResolver{}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			ctx := b.Context()
			b.ReportAllocs()
			for b.Loop() {
				if _, err := approvedTargets(ctx, "tcp", tc.addr, allowPublic, tc.resolver); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
