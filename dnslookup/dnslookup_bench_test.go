// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package dnslookup_test

import (
	"context"
	"net"
	"testing"

	"github.com/altessa-s/go-atlas-net/dnslookup"
)

func BenchmarkLookupMX(b *testing.B) {
	records := []*net.MX{
		{Host: "backup.contoso.com.", Pref: 20},
		{Host: "primary.contoso.com.", Pref: 10},
	}
	l := dnslookup.New(dnslookup.WithResolver(resolverFunc(func(context.Context, string) ([]*net.MX, error) {
		return records, nil
	})))
	ctx := b.Context()

	b.ReportAllocs()

	for b.Loop() {
		_, _ = l.LookupMX(ctx, "Contoso.COM.")
	}
}
