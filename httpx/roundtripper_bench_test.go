// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package httpx_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/httpx"
)

func BenchmarkRoundTripperFunc(b *testing.B) {
	resp := &http.Response{StatusCode: http.StatusOK}
	fn := httpx.RoundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		return resp, nil
	})

	req, err := http.NewRequestWithContext(b.Context(), http.MethodGet, "http://example.com", nil)
	require.NoError(b, err)

	b.ReportAllocs()
	for b.Loop() {
		if _, err := fn.RoundTrip(req); err != nil {
			b.Fatal(err)
		}
	}
}
