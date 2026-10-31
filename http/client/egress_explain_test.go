// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

// TestEgressPolicy_MappedAddressesConnectOverIPv4: an IPv4-mapped address is
// decided as the IPv4 address it connects to — IPv4 rules match it, IPv6
// rules (::/0 included) do not — for allow and deny rules alike.
func TestEgressPolicy_MappedAddressesConnectOverIPv4(t *testing.T) {
	t.Parallel()

	prefix := netip.MustParsePrefix
	tests := []struct {
		name    string
		allow   []httpclient.EgressRule
		deny    []httpclient.EgressRule
		addr    string
		blocked bool
	}{
		{"no rules, mapped public", nil, nil, "[::ffff:93.184.216.34]:443", false},
		{"no rules, mapped private", nil, nil, "[::ffff:10.0.0.1]:443", true},
		{"deny ::/0 skips mapped", nil, []httpclient.EgressRule{{CIDR: prefix("::/0")}}, "[::ffff:93.184.216.34]:443", false},
		{"deny ::/0 refuses ipv6", nil, []httpclient.EgressRule{{CIDR: prefix("::/0")}}, "[2606:2800:220:1::1]:443", true},
		{"deny 0.0.0.0/0 refuses mapped", nil, []httpclient.EgressRule{{CIDR: prefix("0.0.0.0/0")}}, "[::ffff:93.184.216.34]:443", true},
		{"deny ipv4 prefix refuses mapped", nil, []httpclient.EgressRule{{CIDR: prefix("93.184.216.0/24")}}, "[::ffff:93.184.216.34]:443", true},
		{"deny mapped prefix refuses ipv4", nil, []httpclient.EgressRule{{CIDR: prefix("::ffff:93.184.216.0/120")}}, "93.184.216.34:443", true},
		{"allow ::/0 does not exempt mapped private", []httpclient.EgressRule{{CIDR: prefix("::/0")}}, nil, "[::ffff:10.0.0.1]:443", true},
		{"allow ::/0 exempts ipv6 private", []httpclient.EgressRule{{CIDR: prefix("::/0")}}, nil, "[fd00::1]:443", false},
		{"allow ipv4 prefix exempts mapped", []httpclient.EgressRule{{CIDR: prefix("10.0.0.0/8")}}, nil, "[::ffff:10.0.0.1]:443", false},
		{"allow mapped prefix exempts ipv4", []httpclient.EgressRule{{CIDR: prefix("::ffff:10.0.0.0/104")}}, nil, "10.0.0.1:443", false},
		{"deny ipv4 wins over allow ::/0", []httpclient.EgressRule{{CIDR: prefix("::/0")}}, []httpclient.EgressRule{{CIDR: prefix("10.0.0.0/8")}}, "[::ffff:10.0.0.1]:443", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := mustEgressPolicy(t, tc.allow, tc.deny)
			dst := netip.MustParseAddrPort(tc.addr)
			err := p.Check("example.com", dst)
			require.Equal(t, tc.blocked, err != nil, err)
			unmapped := netip.AddrPortFrom(dst.Addr().Unmap(), dst.Port())
			require.Equal(t, err == nil, p.Check("example.com", unmapped) == nil, "the mapped and the IPv4 form decide alike")
		})
	}
}

func TestNewEgressPolicy_ShortMappedPrefix(t *testing.T) {
	t.Parallel()

	_, err := httpclient.NewEgressPolicy(nil, []httpclient.EgressRule{{CIDR: netip.MustParsePrefix("::ffff:10.0.0.0/90")}})
	require.ErrorIs(t, err, httpclient.ErrInvalidEgressRule)
	require.ErrorContains(t, err, "deny[0].cidr")

	// An IPv6 prefix covering the mapped range is valid and matches IPv6 only.
	_, err = httpclient.NewEgressPolicy([]httpclient.EgressRule{{CIDR: netip.MustParsePrefix("::/80")}}, nil)
	require.NoError(t, err)
}

func TestEgressPolicy_Explain(t *testing.T) {
	t.Parallel()

	p := mustEgressPolicy(t,
		[]httpclient.EgressRule{
			{CIDR: netip.MustParsePrefix("10.1.0.0/16")},
			{CIDR: netip.MustParsePrefix("10.2.0.0/16"), Name: "allowPrivate"},
		},
		[]httpclient.EgressRule{
			{Host: "*.internal"},
			{CIDR: netip.MustParsePrefix("10.1.9.0/24"), Port: 22, Name: "deny[7]"},
		},
	)
	tests := []struct {
		name    string
		host    string
		addr    string
		allowed bool
		reason  httpclient.EgressReason
		rule    string
		ip      string
	}{
		{"deny rule", "db.internal", "93.184.216.34:443", false, httpclient.EgressDenyRule, "deny[0] host=*.internal", "93.184.216.34"},
		{"named deny rule", "x", "[::ffff:10.1.9.1]:22", false, httpclient.EgressDenyRule, "deny[7] cidr=10.1.9.0/24 port=22", "10.1.9.1"},
		{"allow rule", "x", "10.1.9.1:443", true, httpclient.EgressAllowRule, "allow[0] cidr=10.1.0.0/16", "10.1.9.1"},
		{"named allow rule, mapped address", "x", "[::ffff:10.2.0.1]:443", true, httpclient.EgressAllowRule, "allowPrivate cidr=10.2.0.0/16", "10.2.0.1"},
		{"private default", "x", "192.168.0.1:443", false, httpclient.EgressPrivateDefault, "", "192.168.0.1"},
		{"public", "x", "[2606:2800:220:1::1%eth0]:443", true, httpclient.EgressPublic, "", "2606:2800:220:1::1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dst := netip.MustParseAddrPort(tc.addr)
			d := p.Explain(tc.host, dst)
			require.Equal(t, tc.allowed, d.Allowed)
			require.Equal(t, tc.reason, d.Reason)
			require.Equal(t, tc.rule, d.Rule)
			require.Equal(t, tc.host, d.Host)
			require.Equal(t, tc.ip, d.IP.String())

			err := p.Check(tc.host, dst)
			require.Equal(t, d.Err(), err, "Check is Explain without the details")
			if !tc.allowed {
				ssrfErr := httpclient.IsSSRFError(err)
				require.NotNil(t, ssrfErr)
				require.Equal(t, tc.rule, ssrfErr.Rule)
				require.Equal(t, tc.ip, ssrfErr.IP)
			}
		})
	}

	t.Run("invalid address", func(t *testing.T) {
		t.Parallel()

		d := p.Explain("x", netip.AddrPort{})
		require.False(t, d.Allowed)
		require.Equal(t, httpclient.EgressInvalidAddress, d.Reason)
		require.False(t, d.IP.IsValid())
		require.ErrorIs(t, d.Err(), httpclient.ErrSSRFBlocked)
	})
}

func TestEgressReason_String(t *testing.T) {
	t.Parallel()

	for reason, want := range map[httpclient.EgressReason]string{
		httpclient.EgressInvalidAddress: "invalid_address",
		httpclient.EgressDenyRule:       "deny_rule",
		httpclient.EgressAllowRule:      "allow_rule",
		httpclient.EgressPrivateDefault: "private_default",
		httpclient.EgressPublic:         "public",
		httpclient.EgressReason(0):      "EgressReason(0)",
	} {
		require.Equal(t, want, reason.String())
	}
}

func TestEgressPolicy_CheckHost(t *testing.T) {
	t.Parallel()

	p := mustEgressPolicy(t, nil, []httpclient.EgressRule{
		{Host: "*.internal"},
		{Host: "mail.example", Port: 25},
		{CIDR: netip.MustParsePrefix("93.184.216.0/24")},
		{CIDR: netip.MustParsePrefix("10.0.0.0/8"), Host: "db.example"},
	})
	tests := []struct {
		name string
		host string
		port uint16
		rule string // "-" allowed, "" refused without a rule
	}{
		{"host rule", "db.internal", 443, "deny[0] host=*.internal"},
		{"host rule, case and trailing dot", "DB.Internal.", 443, "deny[0] host=*.internal"},
		{"host and port rule", "mail.example", 25, "deny[1] host=mail.example port=25"},
		{"host and port rule, other port", "mail.example", 587, "-"},
		{"cidr rule waits for the address", "www.example", 443, "-"},
		{"cidr and host rule waits for the address", "db.example", 443, "-"},
		{"literal ip in a deny cidr", "93.184.216.34", 443, "deny[2] cidr=93.184.216.0/24"},
		{"mapped literal in an ipv4 deny cidr", "::ffff:93.184.216.34", 443, "deny[2] cidr=93.184.216.0/24"},
		{"literal private ip", "169.254.169.254", 80, ""},
		{"literal with unicode full stops", "127。0。0。1", 80, ""},
		{"literal public ip", "8.8.8.8", 443, "-"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := p.CheckHost(tc.host, tc.port)
			if tc.rule == "-" {
				require.NoError(t, err)
				return
			}
			ssrfErr := httpclient.IsSSRFError(err)
			require.NotNil(t, ssrfErr, err)
			require.Equal(t, tc.host, ssrfErr.Host)
			require.Equal(t, tc.rule, ssrfErr.Rule)
		})
	}
}

func TestNewEgressPolicy_Name(t *testing.T) {
	t.Parallel()

	t.Run("a name is not a matcher", func(t *testing.T) {
		t.Parallel()

		_, err := httpclient.NewEgressPolicy(nil, []httpclient.EgressRule{{Name: "blocked"}})
		require.ErrorIs(t, err, httpclient.ErrInvalidEgressRule)
		require.ErrorContains(t, err, "deny[0]")
	})

	for _, name := range []string{"block metadata", "deny:0", "a/b", "token=secret", "x?y", strings.Repeat("a", 65), "bücher"} {
		t.Run("unsafe "+name, func(t *testing.T) {
			t.Parallel()

			_, err := httpclient.NewEgressPolicy(nil, []httpclient.EgressRule{{Port: 22, Name: name}})
			require.ErrorIs(t, err, httpclient.ErrInvalidEgressRule)
			require.ErrorContains(t, err, "deny[0].name")
		})
	}

	for _, name := range []string{"blockMetadata", "deny[7]", "allow.private_hosts-2", strings.Repeat("a", 64)} {
		t.Run("safe "+name, func(t *testing.T) {
			t.Parallel()

			_, err := httpclient.NewEgressPolicy(nil, []httpclient.EgressRule{{Port: 22, Name: name}})
			require.NoError(t, err)
		})
	}
}

// TestWithEgressPolicy_NamedRuleSurvivesRedaction: the client's error for a
// refusal keeps the label of a named deny rule through its redaction.
func TestWithEgressPolicy_NamedRuleSurvivesRedaction(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(srv.Close)
	p := mustEgressPolicy(t, loopbackAllow, []httpclient.EgressRule{{CIDR: netip.MustParsePrefix("127.0.0.0/8"), Name: "blockLoopback"}})
	c := httpclient.New(httpclient.WithEgressPolicy(p), httpclient.WithRetryMax(0))
	_, err := get(t, c, srv.URL+"/?token=secret-value")
	err = httpclient.RedactError(err) // the plain *http.Client's error, as callers redact it
	require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)
	require.Contains(t, err.Error(), "blockLoopback cidr=127.0.0.0/8")
	require.NotContains(t, err.Error(), "(unlabeled rule)")
	require.NotContains(t, err.Error(), "secret-value")
}

func TestNormalizeEgressHostGlob(t *testing.T) {
	t.Parallel()

	for pattern, want := range map[string]string{
		"*.Example.ORG":               "*.example.org",
		"Bücher.example":              "xn--bcher-kva.example",
		"*.bücher。example":            "*.xn--bcher-kva.example",
		"a｡b．c":                       "a.b.c",
		"[A-C].xn--bcher-kva.example": "[a-c].xn--bcher-kva.example",
		"db?.internal":                "db?.internal",
	} {
		got, err := httpclient.NormalizeEgressHostGlob(pattern)
		require.NoError(t, err, pattern)
		require.Equal(t, want, got, pattern)
	}
	for _, pattern := range []string{
		"",
		"example.­",   // the soft hyphen label maps to nothing: "example."
		"­",           // normalizes to an empty glob
		"a.­.example", // an empty label
		"a.example。",  // a trailing Unicode full stop
		"bü*.example", // Unicode and glob metacharacters
		"user@example.com",
		"example.com.",
		"[a-",
	} {
		got, err := httpclient.NormalizeEgressHostGlob(pattern)
		require.ErrorIs(t, err, httpclient.ErrInvalidEgressRule, "%q -> %q", pattern, got)
		_, err = httpclient.NewEgressPolicy(nil, []httpclient.EgressRule{{Host: pattern}})
		if pattern != "" { // an empty host is no matcher, rejected as such
			require.ErrorIs(t, err, httpclient.ErrInvalidEgressRule, pattern)
		}
	}
}
