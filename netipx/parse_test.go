// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package netipx_test

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/netipx"
)

func TestParsePrefixes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      []string
		want    []netip.Prefix
		wantErr bool
	}{
		{name: "nil input", in: nil, want: []netip.Prefix{}},
		{name: "empty input", in: []string{}, want: []netip.Prefix{}},
		{
			name: "IPv4 CIDR",
			in:   []string{"10.0.0.0/8"},
			want: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		},
		{
			name: "IPv6 CIDR",
			in:   []string{"fd00::/8"},
			want: []netip.Prefix{netip.MustParsePrefix("fd00::/8")},
		},
		{
			name: "CIDR host bits are kept as written",
			in:   []string{"10.1.2.3/8"},
			want: []netip.Prefix{netip.MustParsePrefix("10.1.2.3/8")},
		},
		{
			name: "bare IPv4 becomes host prefix",
			in:   []string{"192.168.1.1"},
			want: []netip.Prefix{netip.MustParsePrefix("192.168.1.1/32")},
		},
		{
			name: "bare IPv6 becomes host prefix",
			in:   []string{"2001:db8::1"},
			want: []netip.Prefix{netip.MustParsePrefix("2001:db8::1/128")},
		},
		{
			name: "order is preserved",
			in:   []string{"8.8.8.8", "10.0.0.0/8", "::1"},
			want: []netip.Prefix{
				netip.MustParsePrefix("8.8.8.8/32"),
				netip.MustParsePrefix("10.0.0.0/8"),
				netip.MustParsePrefix("::1/128"),
			},
		},
		{name: "empty element", in: []string{""}, wantErr: true},
		{name: "surrounding whitespace is not trimmed", in: []string{" 10.0.0.1"}, wantErr: true},
		{name: "malformed address", in: []string{"10.0.0.256"}, wantErr: true},
		{name: "prefix length out of range", in: []string{"10.0.0.0/33"}, wantErr: true},
		{name: "hostname", in: []string{"example.com"}, wantErr: true},
		{name: "one bad element fails the batch", in: []string{"10.0.0.0/8", "garbage"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := netipx.ParsePrefixes(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				require.Nil(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// FuzzParsePrefixes checks that arbitrary input never panics and that a
// successful parse yields one valid prefix per element, with bare addresses
// mapped to full-length host prefixes.
func FuzzParsePrefixes(f *testing.F) {
	for _, seed := range []string{"10.0.0.0/8", "192.168.1.1", "2001:db8::1", "fe80::1%eth0", "", " ", "10.0.0.0/33", "garbage"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		elems := strings.Split(in, ",")
		got, err := netipx.ParsePrefixes(elems)
		if err != nil {
			require.Nil(t, got)
			return
		}
		require.Len(t, got, len(elems))
		for i, p := range got {
			require.True(t, p.IsValid(), "prefix %q from %q is invalid", p, elems[i])
			if addr, aerr := netip.ParseAddr(elems[i]); aerr == nil {
				require.Equal(t, addr.BitLen(), p.Bits(), "bare address %q is not a host prefix", elems[i])
			}
		}
	})
}
