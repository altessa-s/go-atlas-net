// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package egressrule_test

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/egressrule"
)

func TestRuleParse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		list    egressrule.List
		rule    egressrule.Rule
		want    egressrule.Matcher
		wantErr string
	}{
		{name: "deny host", list: egressrule.Deny, rule: egressrule.Rule{Host: "*.Internal"}, want: egressrule.Matcher{Host: "*.internal"}},
		{name: "deny port", list: egressrule.Deny, rule: egressrule.Rule{Port: 22}, want: egressrule.Matcher{Port: 22}},
		{
			name: "deny named cidr", list: egressrule.Deny, rule: egressrule.Rule{Name: "meta", CIDR: "169.254.169.254/32"},
			want: egressrule.Matcher{Name: "meta", CIDR: netip.MustParsePrefix("169.254.169.254/32")},
		},
		{
			name: "allow cidr masked", list: egressrule.Allow, rule: egressrule.Rule{CIDR: "10.1.2.3/8", Host: "a.example", Port: 443},
			want: egressrule.Matcher{CIDR: netip.MustParsePrefix("10.0.0.0/8"), Host: "a.example", Port: 443},
		},
		{
			name: "mapped prefix becomes IPv4", list: egressrule.Allow, rule: egressrule.Rule{CIDR: "::ffff:10.0.0.0/104"},
			want: egressrule.Matcher{CIDR: netip.MustParsePrefix("10.0.0.0/8")},
		},
		{
			name: "unicode host to punycode", list: egressrule.Deny, rule: egressrule.Rule{Host: "bücher。example"},
			want: egressrule.Matcher{Host: "xn--bcher-kva.example"},
		},
		{name: "deny without matcher", list: egressrule.Deny, rule: egressrule.Rule{Name: "x"}, wantErr: "deny[0]: at least one matcher is required"},
		{name: "allow without cidr", list: egressrule.Allow, rule: egressrule.Rule{Host: "a.example"}, wantErr: "allow[0].cidr: required"},
		{name: "bad cidr", list: egressrule.Allow, rule: egressrule.Rule{CIDR: "10.0.0.0"}, wantErr: "allow[0].cidr:"},
		{name: "short mapped prefix", list: egressrule.Allow, rule: egressrule.Rule{CIDR: "::ffff:0:0/64"}, wantErr: "IPv4-mapped prefix"},
		{name: "negative port", list: egressrule.Deny, rule: egressrule.Rule{Port: -1}, wantErr: "deny[0].port"},
		{name: "port too large", list: egressrule.Deny, rule: egressrule.Rule{Port: 65536}, wantErr: "deny[0].port"},
		{name: "bad name", list: egressrule.Deny, rule: egressrule.Rule{Name: "a b", Port: 1}, wantErr: "deny[0].name"},
		{name: "long name", list: egressrule.Deny, rule: egressrule.Rule{Name: strings.Repeat("a", 65), Port: 1}, wantErr: "deny[0].name"},
		{name: "host with scheme", list: egressrule.Deny, rule: egressrule.Rule{Host: "http://a"}, wantErr: "deny[0].host"},
		{name: "host with port", list: egressrule.Deny, rule: egressrule.Rule{Host: "a:80"}, wantErr: "deny[0].host"},
		{name: "host trailing dot", list: egressrule.Deny, rule: egressrule.Rule{Host: "a."}, wantErr: "deny[0].host"},
		{name: "malformed glob", list: egressrule.Deny, rule: egressrule.Rule{Host: "[a"}, wantErr: "deny[0].host"},
		{name: "unicode glob label", list: egressrule.Deny, rule: egressrule.Rule{Host: "bü*.example"}, wantErr: "punycode"},
		{name: "soft hyphen label", list: egressrule.Deny, rule: egressrule.Rule{Host: "example.\u00ad"}, wantErr: "deny[0].host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.rule.Parse(tc.list, 0)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestCheckUnknownList(t *testing.T) {
	t.Parallel()
	_, err := egressrule.Check("other", 0, egressrule.Matcher{Port: 1})
	require.ErrorContains(t, err, "unknown list")
}

func TestValidateDenyFirst(t *testing.T) {
	t.Parallel()
	require.NoError(t, egressrule.Validate(nil, nil))
	err := egressrule.Validate([]egressrule.Rule{{}}, []egressrule.Rule{{CIDR: "10.0.0.0/8"}, {}})
	require.ErrorContains(t, err, "deny[1]")
}

func TestNormalizeHost(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"Example.COM.":   "example.com",
		"bücher.example": "xn--bcher-kva.example",
		"127。0。0。1":      "127.0.0.1",
	} {
		require.Equal(t, want, egressrule.NormalizeHost(in), in)
	}
}

func TestNormalizePrefix(t *testing.T) {
	t.Parallel()
	require.Equal(t, netip.MustParsePrefix("0.0.0.0/0"), egressrule.NormalizePrefix(netip.MustParsePrefix("::ffff:0:0/96")))
	require.Equal(t, netip.MustParsePrefix("fd00::/8"), egressrule.NormalizePrefix(netip.MustParsePrefix("fd12::/8")))
}
