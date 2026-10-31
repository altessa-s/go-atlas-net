// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package metadata

import (
	"testing"

	"github.com/stretchr/testify/require"

	grpcmd "google.golang.org/grpc/metadata"
)

func TestHeaderGetter_GetHeader(t *testing.T) {
	md := grpcmd.Pairs("x-forwarded-for", "1.2.3.4", "x-forwarded-for", "5.6.7.8")
	ctx := grpcmd.NewIncomingContext(t.Context(), md)

	g := NewHeaderGetter(ctx)
	vals := g.GetHeader("x-forwarded-for")
	require.Len(t, vals, 2)
}

func TestHeaderGetter_GetSingleHeader(t *testing.T) {
	md := grpcmd.Pairs("request-id", "abc123")
	ctx := grpcmd.NewIncomingContext(t.Context(), md)

	g := NewHeaderGetter(ctx)
	got := g.GetSingleHeader("request-id")
	require.Equal(t, "abc123", got)
}

func TestHeaderGetter_NilReceiver(t *testing.T) {
	var g *HeaderGetter
	vals := g.GetHeader("x-test")
	require.Nil(t, vals)
	got := g.GetSingleHeader("x-test")
	require.Equal(t, "", got)
}

func TestHeaderGetter_NilContext(t *testing.T) {
	g := &HeaderGetter{ctx: nil}
	vals := g.GetHeader("x-test")
	require.Nil(t, vals)
}

func TestHeaderGetter_MissingHeader(t *testing.T) {
	ctx := grpcmd.NewIncomingContext(t.Context(), grpcmd.New(nil))
	g := NewHeaderGetter(ctx)

	got := g.GetSingleHeader("x-missing")
	require.Equal(t, "", got)
}
