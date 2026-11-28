// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package dnslookup_test

import (
	"context"
	"net"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/dnslookup"
	"github.com/altessa-s/go-atlas-net/egressrule"
)

func FuzzLookupMX_QueryName(f *testing.F) {
	for _, name := range []string{"Contoso.COM.", "bücher。com。", "mail", "contoso.com..", "localhost", "host/other.com", "192.0.2.1"} {
		f.Add(name)
	}

	absoluteName := regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+$`)

	f.Fuzz(func(t *testing.T, input string) {
		var calls int

		l := dnslookup.New(dnslookup.WithResolver(resolverFunc(func(_ context.Context, name string) ([]*net.MX, error) {
			calls++

			require.Regexp(t, absoluteName, name)
			require.LessOrEqual(t, len(name), 254)
			require.Equal(t, egressrule.NormalizeHost(input), strings.TrimSuffix(name, "."))

			return nil, nil
		})))

		_, err := l.LookupMX(t.Context(), input)
		if err != nil {
			require.ErrorIs(t, err, dnslookup.ErrRefused)
			require.Zero(t, calls)

			return
		}

		require.Equal(t, 1, calls)
	})
}
