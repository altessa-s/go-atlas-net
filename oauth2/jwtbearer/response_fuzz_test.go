// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package jwtbearer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func FuzzTokenResponse(f *testing.F) {
	f.Add([]byte(`{"access_token":"secret","expires_in":"3600"}`))
	f.Add([]byte(`{"access_token":"secret","expires_in":99999999999999999999999999}`))
	f.Add([]byte(`{"access_token":"secret","expires_in":-1}`))

	now := time.Unix(0, 0)

	opts := defaultOptions()

	f.Fuzz(func(t *testing.T, data []byte) {
		tok, err := parseResponse(data, now, opts)
		if err != nil {
			require.Equal(t, ErrResponse, err)
			require.Zero(t, tok)

			return
		}

		require.NotEmpty(t, tok.AccessToken.Expose())
		require.True(t, tok.Expiry.After(now))
	})
}
