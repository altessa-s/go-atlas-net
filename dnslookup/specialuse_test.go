// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package dnslookup

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

type unexpectedResolver struct{ t *testing.T }

func (r unexpectedResolver) LookupMX(context.Context, string) ([]*net.MX, error) {
	r.t.Helper()
	r.t.Fatal("resolver called for special-use name")

	return nil, nil
}

func TestLookupMX_SpecialUseSnapshot(t *testing.T) {
	t.Parallel()

	require.Len(t, specialUseDomains, 42)

	for domain := range specialUseDomains {
		t.Run(domain, func(t *testing.T) {
			t.Parallel()

			l := New(WithResolver(unexpectedResolver{t: t}))
			for _, name := range []string{domain, "mx." + domain + "."} {
				mx, err := l.LookupMX(t.Context(), name)
				require.ErrorIs(t, err, ErrRefused)
				require.Empty(t, mx)
			}
		})
	}
}
