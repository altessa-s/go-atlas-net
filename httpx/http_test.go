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

func TestRoundTripperFunc(t *testing.T) {
	t.Parallel()

	called := false
	fn := httpx.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: 200}, nil
	})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com", nil)
	require.NoError(t, err)

	resp, err := fn.RoundTrip(req)

	require.NoError(t, err, "RoundTrip failed")
	require.True(t, called, "RoundTripperFunc not called")
	require.Equal(t, 200, resp.StatusCode)
}
