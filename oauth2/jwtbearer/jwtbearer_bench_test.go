// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package jwtbearer_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/httpx"
	"github.com/altessa-s/go-atlas-net/oauth2/jwtbearer"
)

func BenchmarkCachedToken(b *testing.B) {
	doer := httpx.DoerFunc(func(*http.Request) (*http.Response, error) {
		return response(200, `{"access_token":"value","expires_in":3600}`), nil
	})
	s, err := jwtbearer.New(doer, endpoint, "issuer", keyFor(b))
	require.NoError(b, err)
	_, err = s.Token(b.Context())
	require.NoError(b, err)
	ctx := b.Context()
	b.ReportAllocs()

	for b.Loop() {
		_, err = s.Token(ctx)
		if err != nil {
			b.Fatal(err)
		}
	}
}
