// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package limiters_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/http/client/limiters"
	"github.com/altessa-s/go-atlas-net/httpx"
)

// idleCloser is a transport that counts CloseIdleConnections calls.
type idleCloser struct {
	httpx.RoundTripperFunc

	closed int
}

func (c *idleCloser) CloseIdleConnections() { c.closed++ }

func TestRoundTripper_CloseIdleConnections(t *testing.T) {
	t.Parallel()

	t.Run("forwards_to_next", func(t *testing.T) {
		t.Parallel()

		next := &idleCloser{}
		client := &http.Client{Transport: limiters.NewRoundTripper(next, limiters.Chain())}
		client.CloseIdleConnections()
		client.CloseIdleConnections()
		require.Equal(t, 2, next.closed, "http.Client.CloseIdleConnections must reach the pooled transport")
	})

	t.Run("next_without_pool", func(t *testing.T) {
		t.Parallel()

		next := httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, http.ErrNotSupported })
		rt := limiters.NewRoundTripper(next, nil)
		require.NotPanics(t, rt.CloseIdleConnections)
	})
}
