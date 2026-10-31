// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package metadata

import (
	"testing"

	grpcmd "google.golang.org/grpc/metadata"
)

func BenchmarkHeaderGetter_GetSingleHeader(b *testing.B) {
	md := grpcmd.Pairs("request-id", "abc123")
	ctx := grpcmd.NewIncomingContext(b.Context(), md)
	g := NewHeaderGetter(ctx)

	for b.Loop() {
		g.GetSingleHeader("request-id")
	}
}
