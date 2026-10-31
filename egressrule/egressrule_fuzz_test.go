// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package egressrule_test

import (
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/egressrule"
)

func TestNormalizeHostGlob(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pattern string
		want    string
		wantErr bool
	}{
		{name: "lower-cased", pattern: "API.Example.COM", want: "api.example.com"},
		{name: "wildcard kept", pattern: "*.example.com", want: "*.example.com"},
		{name: "character class kept", pattern: "api-[0-9].example.com", want: "api-[0-9].example.com"},
		{name: "unicode label to punycode", pattern: "bücher.de", want: "xn--bcher-kva.de"},
		{name: "ideographic full stop", pattern: "example。com", want: "example.com"},
		{name: "fullwidth full stop", pattern: "example．com", want: "example.com"},
		{name: "empty", pattern: "", wantErr: true},
		{name: "scheme", pattern: "https://example.com", wantErr: true},
		{name: "port", pattern: "example.com:443", wantErr: true},
		{name: "path", pattern: "example.com/a", wantErr: true},
		{name: "user info", pattern: "user@example.com", wantErr: true},
		{name: "space", pattern: "example .com", wantErr: true},
		{name: "trailing dot", pattern: "example.com.", wantErr: true},
		{name: "trailing unicode dot", pattern: "example.com。", wantErr: true},
		{name: "malformed glob", pattern: "[example.com", wantErr: true},
		{name: "unicode label with glob", pattern: "bü*.de", wantErr: true},
		{name: "label mapping to nothing", pattern: "example.\u00ad", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := egressrule.NormalizeHostGlob(tc.pattern)
			if tc.wantErr {
				require.Error(t, err)
				require.Empty(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	require.NoError(t, egressrule.Validate(
		[]egressrule.Rule{{CIDR: "10.0.0.0/8", Port: 443}},
		[]egressrule.Rule{{Host: "*.internal"}, {CIDR: "169.254.169.254/32"}},
	))
	require.ErrorContains(t, egressrule.Validate([]egressrule.Rule{{Host: "x.test"}}, nil), "allow[0].cidr")
	require.ErrorContains(t, egressrule.Validate(nil, []egressrule.Rule{{}, {Port: 70000}}), "deny[0]")
	require.ErrorContains(t, egressrule.Validate(nil, []egressrule.Rule{{Port: 70000}}), "deny[0].port")
	require.ErrorContains(t, egressrule.Validate(nil, []egressrule.Rule{{Name: "bad name", Port: 1}}), "deny[0].name")
}

// requireNormalizedGlob asserts the invariants of a host glob returned by
// [egressrule.NormalizeHostGlob].
func requireNormalizedGlob(t *testing.T, host string) {
	t.Helper()

	require.NotEmpty(t, host)
	for i := range len(host) {
		require.Less(t, host[i], byte(0x80), "non-ASCII byte in %q", host)
		require.False(t, host[i] >= 'A' && host[i] <= 'Z', "upper case in %q", host)
	}
	require.False(t, strings.ContainsAny(host, ":/@ \t"), "forbidden character in %q", host)
	require.False(t, strings.HasSuffix(host, "."), "trailing dot in %q", host)
	_, err := path.Match(host, "")
	require.NoError(t, err)

	again, err := egressrule.NormalizeHostGlob(host)
	require.NoError(t, err)
	require.Equal(t, host, again, "not idempotent")
}

func FuzzNormalizeHostGlob(f *testing.F) {
	for _, seed := range []string{
		"example.com", "*.Example.COM", "api-[0-9].example.com", "bücher.de", "example。com", "example.com.",
		"https://x", "x:80", "[x", "bü*.de", "example.\u00ad", "xn--bcher-kva.de", "a\\b", "",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, pattern string) {
		host, err := egressrule.NormalizeHostGlob(pattern)
		if err != nil {
			require.Empty(t, host)
			return
		}
		requireNormalizedGlob(t, host)
	})
}

func FuzzRuleParse(f *testing.F) {
	f.Add("deny", "metadata", "*.internal", "169.254.169.254/32", 80)
	f.Add("allow", "", "", "10.0.0.0/8", 0)
	f.Add("allow", "", "x.test", "::ffff:10.0.0.0/104", 443)
	f.Add("deny", "bad name", "", "", -1)
	f.Add("other", "", "x", "300.0.0.0/8", 70000)
	f.Fuzz(func(t *testing.T, list, name, host, cidr string, port int) {
		m, err := egressrule.Rule{Name: name, Host: host, CIDR: cidr, Port: port}.Parse(egressrule.List(list), 0)
		if err != nil {
			require.Zero(t, m)
			return
		}
		require.True(t, list == string(egressrule.Allow) || list == string(egressrule.Deny))
		require.Equal(t, uint16(port), m.Port) //nolint:gosec // a parsed port is within 0 to 65535
		if m.Host != "" {
			requireNormalizedGlob(t, m.Host)
		}
		if m.CIDR.IsValid() {
			require.Equal(t, m.CIDR.Masked(), m.CIDR, "prefix not masked")
			require.False(t, m.CIDR.Addr().Is4In6(), "IPv4-mapped prefix not unmapped")
		}
		again, err := egressrule.Check(egressrule.List(list), 0, m)
		require.NoError(t, err)
		require.Equal(t, m, again, "Check of a parsed matcher is not idempotent")
	})
}

func FuzzNormalizeHost(f *testing.F) {
	for _, seed := range []string{"Example.COM.", "bücher.de", "example。com。", "\u00ad", "a..", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, host string) {
		got := egressrule.NormalizeHost(host)
		for i := range len(got) {
			require.False(t, got[i] >= 'A' && got[i] <= 'Z', "upper case in %q", got)
		}
	})
}
