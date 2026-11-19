// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

// TestIdempotencyKey_ConnectionClosedBeforeReply sends a POST to a server that
// reads it and drops the connection before any reply: the request may have
// been processed, so only a POST with exactly one idempotency key is resent.
func TestIdempotencyKey_ConnectionClosedBeforeReply(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		header   http.Header
		ctx      func(context.Context) context.Context
		wantHits int32
	}{
		{name: "single key is retried", header: http.Header{"Idempotency-Key": {"order-1"}}, wantHits: 3},
		{name: "duplicate key values", header: http.Header{"Idempotency-Key": {"order-1", "order-2"}}, wantHits: 1},
		{name: "both header names", header: http.Header{
			"Idempotency-Key": {"order-1"}, "X-Idempotency-Key": {"order-1"},
		}, wantHits: 1},
		{name: "blank key", header: http.Header{"Idempotency-Key": {"  "}}, wantHits: 1},
		{name: "non-canonical duplicate map key", header: http.Header{
			"Idempotency-Key": {"order-1"}, "idempotency-key": {"order-1"},
		}, wantHits: 1},
		{
			name:     "single key under ContextWithoutRetry",
			header:   http.Header{"Idempotency-Key": {"order-1"}},
			ctx:      httpclient.ContextWithoutRetry,
			wantHits: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var hits atomic.Int32

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)

				_, _ = io.Copy(io.Discard, r.Body)

				conn, _, err := http.NewResponseController(w).Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}

				_ = conn.Close()
			}))
			t.Cleanup(srv.Close)

			c := httpclient.New(loopback, httpclient.WithRetryMax(2), httpclient.WithRetryWait(time.Millisecond, time.Millisecond))

			ctx := t.Context()
			if tc.ctx != nil {
				ctx = tc.ctx(ctx)
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, strings.NewReader("order"))
			require.NoError(t, err)

			req.Header = tc.header

			resp, err := c.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}

			require.Error(t, err)
			require.Equal(t, tc.wantHits, hits.Load())
		})
	}
}
