// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client_test

import (
	"net/netip"
	"testing"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

// BenchmarkEgressPolicy_Check measures the per-connection decision made in
// the dial control hook, with no rules (default private denial only) and
// with a realistic rule set.
func BenchmarkEgressPolicy_Check(b *testing.B) {
	empty := mustEgressPolicy(b, nil, nil)
	rules := mustEgressPolicy(b,
		[]httpclient.EgressRule{
			{CIDR: netip.MustParsePrefix("10.20.0.0/16")},
			{CIDR: netip.MustParsePrefix("10.30.0.0/16"), Host: "*.corp.example", Port: 443},
		},
		[]httpclient.EgressRule{
			{Host: "*.internal"},
			{CIDR: netip.MustParsePrefix("203.0.113.0/24")},
			{Port: 25},
			{Host: "api.example.com", Port: 8080},
		},
	)
	cases := []struct {
		name   string
		policy *httpclient.EgressPolicy
		host   string
		dst    netip.AddrPort
	}{
		{"no_rules_public", empty, "example.com", netip.MustParseAddrPort("93.184.216.34:443")},
		{"no_rules_private", empty, "example.com", netip.MustParseAddrPort("10.0.0.1:443")},
		{"rules_public", rules, "example.com", netip.MustParseAddrPort("93.184.216.34:443")},
		{"rules_allowed_private", rules, "svc.corp.example", netip.MustParseAddrPort("10.30.1.1:443")},
		{"rules_denied_host", rules, "db.prod.internal", netip.MustParseAddrPort("93.184.216.34:443")},
		{"rules_mapped_upper_host", rules, "WWW.Example.COM", netip.MustParseAddrPort("[::ffff:93.184.216.34]:443")},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = tc.policy.Check(tc.host, tc.dst)
			}
		})
	}
}
