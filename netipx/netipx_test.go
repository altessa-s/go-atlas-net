// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package netipx

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsPrivate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ip   string
		want bool
	}{
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"127.0.0.1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"198.18.0.1", true},      // RFC 2544 benchmarking, first /16
		{"198.19.255.254", true},  // RFC 2544 benchmarking, second /16
		{"198.20.0.1", false},     // just past the benchmarking block
		{"192.18.0.1", false},     // public, not the benchmarking block
		{"192.19.255.254", false}, // public, not the benchmarking block
		{"::1", true},
		{"2001:db8::1", true},
		{"2607:f8b0:4004:800::200e", false},
		{"::ffff:10.0.0.1", true},             // IPv4-mapped RFC 1918
		{"::ffff:127.0.0.1", true},            // IPv4-mapped loopback
		{"::ffff:8.8.8.8", false},             // IPv4-mapped public
		{"fe80::1%en0", true},                 // zoned link-local
		{"fd00::1%eth0", true},                // zoned unique-local
		{"2607:f8b0:4004:800::200e%1", false}, // zoned public
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			t.Parallel()
			addr := netip.MustParseAddr(tt.ip)
			got := IsPrivate(addr)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestIsTrusted(t *testing.T) {
	t.Parallel()
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}

	tests := []struct {
		ip   string
		want bool
	}{
		{"10.0.0.1", true},
		{"192.168.1.1", false},
		{"8.8.8.8", false},
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			t.Parallel()
			addr := netip.MustParseAddr(tt.ip)
			got := IsTrusted(addr, trusted)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestInList(t *testing.T) {
	t.Parallel()
	require.False(t, InList(netip.MustParseAddr("1.2.3.4"), nil), "InList with nil prefixes should return false")
}

func TestIsPrivate_SpecialPurposeIPv6(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"64:ff9b::a9fe:a9fe", true},   // NAT64 well-known prefix → 169.254.169.254
		{"64:ff9b::a00:1", true},       // NAT64 → 10.0.0.1
		{"64:ff9b::7f00:1", true},      // NAT64 → 127.0.0.1
		{"64:ff9b::808:808", false},    // NAT64 → 8.8.8.8
		{"64:ff9b:1::a9fe:a9fe", true}, // RFC 8215 local-use NAT64
		{"64:ff9b:1:ffff::1", true},
		{"::a9fe:a9fe", true}, // deprecated IPv4-compatible
		{"::808:808", true},
		{"3fff::1", true}, // RFC 9637 documentation
		{"5f00::1", true}, // RFC 9602 SRv6 SIDs
		{"fec0::1", true}, // RFC 3879 deprecated site-local
		{"2606:4700:4700::1111", false},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, IsPrivate(netip.MustParseAddr(tc.addr)))
		})
	}
}
