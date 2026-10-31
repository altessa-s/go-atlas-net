// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client_test

import (
	"net/netip"
	"strconv"
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

// benchManyRulesPolicy returns a policy with n deny host globs and n allow
// CIDRs, the size of a generated per-tenant configuration.
func benchManyRulesPolicy(b *testing.B, n int) *httpclient.EgressPolicy {
	b.Helper()
	allow := make([]httpclient.EgressRule, 0, n)
	deny := make([]httpclient.EgressRule, 0, n)
	for i := range n {
		allow = append(allow, httpclient.EgressRule{CIDR: netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(i), 0, 0}), 16), Port: 443})
		deny = append(deny, httpclient.EgressRule{Host: "*.svc" + strconv.Itoa(i) + ".internal"})
	}
	return mustEgressPolicy(b, allow, deny)
}

func BenchmarkEgressPolicy_CheckManyRules(b *testing.B) {
	for _, n := range []int{8, 64, 256} {
		p := benchManyRulesPolicy(b, n)
		b.Run("rules_"+strconv.Itoa(n)+"/public", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = p.Check("api.example.com", netip.MustParseAddrPort("93.184.216.34:443"))
			}
		})
		b.Run("rules_"+strconv.Itoa(n)+"/last_allow", func(b *testing.B) {
			dst := netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, byte(n - 1), 1, 1}), 443)
			b.ReportAllocs()
			for b.Loop() {
				_ = p.Check("svc.corp.example", dst)
			}
		})
	}
}

func BenchmarkEgressPolicy_CheckHost(b *testing.B) {
	p := benchManyRulesPolicy(b, 64)
	cases := []struct {
		name string
		host string
	}{
		{"public_name", "api.example.com"},
		{"denied_name", "db.svc63.internal"},
		{"literal_ipv4", "10.20.0.1"},
		{"literal_ipv6", "[2001:db8::1]"},
		{"unicode_name", "bücher.example"},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = p.CheckHost(tc.host, 443)
			}
		})
	}
}

func BenchmarkEgressPolicy_Explain(b *testing.B) {
	p := benchManyRulesPolicy(b, 64)
	cases := []struct {
		name string
		host string
		dst  netip.AddrPort
	}{
		{"public", "api.example.com", netip.MustParseAddrPort("93.184.216.34:443")},
		{"deny_rule", "db.svc10.internal", netip.MustParseAddrPort("93.184.216.34:443")},
		{"allow_rule", "svc.corp.example", netip.MustParseAddrPort("10.5.1.1:443")},
		{"private_default", "svc.corp.example", netip.MustParseAddrPort("192.168.1.1:443")},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = p.Explain(tc.host, tc.dst)
			}
		})
	}
}
