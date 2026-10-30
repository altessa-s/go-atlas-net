// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package clientip_test

import (
	"net/http"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/clientip"
	"github.com/altessa-s/go-atlas-net/grpc/metadata"
	"github.com/altessa-s/go-atlas-net/httpx"

	grpcmd "google.golang.org/grpc/metadata"
)

// The transport adapters satisfy the consumer-side interface implicitly.
var (
	_ clientip.HeaderGetter = (*httpx.HTTPHeaderGetter)(nil)
	_ clientip.HeaderGetter = (*metadata.HeaderGetter)(nil)
)

func TestExtract_HTTPHeaderGetter(t *testing.T) {
	t.Parallel()

	h := http.Header{}
	h.Add("X-Forwarded-For", "1.1.1.1")
	h.Add("x-forwarded-for", "8.8.8.8")

	e, err := clientip.NewExtractor(clientip.WithTrustedPeers(netip.MustParsePrefix("10.0.0.0/8")))
	require.NoError(t, err)
	got := e.Extract(t.Context(), netip.MustParseAddr("10.0.0.1"), httpx.NewHTTPHeaderGetter(&http.Request{Header: h}))
	require.Equal(t, "8.8.8.8", got.String())
}

func TestExtract_GRPCHeaderGetter(t *testing.T) {
	t.Parallel()

	ctx := grpcmd.NewIncomingContext(t.Context(), grpcmd.Pairs("x-forwarded-for", "1.1.1.1, 8.8.8.8"))

	e, err := clientip.NewExtractor(clientip.WithTrustedPeers(netip.MustParsePrefix("10.0.0.0/8")))
	require.NoError(t, err)
	got := e.Extract(ctx, netip.MustParseAddr("10.0.0.1"), metadata.NewHeaderGetter(ctx))
	require.Equal(t, "8.8.8.8", got.String())
}
