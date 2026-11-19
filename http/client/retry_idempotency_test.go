// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
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

// TestIdempotencyKey_ReusedConnectionClosedBeforeReply warms an HTTP/1.1
// connection, then sends a POST over it that the server reads and answers by
// closing the connection. net/http resends a request with an idempotency key
// header after such a failure on a reused connection unless the request is
// not rewindable; the retry layer hands a request it sends once with a body
// on without GetBody, so the server sees it once. A bodyless one is resent by
// net/http (the documented caveat).
func TestIdempotencyKey_ReusedConnectionClosedBeforeReply(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		header   http.Header
		body     bool
		ctx      func(context.Context) context.Context
		wantHits int32
	}{
		{name: "duplicate key values", header: http.Header{"Idempotency-Key": {"order-1", "order-2"}}, body: true, wantHits: 1},
		{name: "blank key", header: http.Header{"Idempotency-Key": {""}}, body: true, wantHits: 1},
		{name: "both header names", header: http.Header{
			"Idempotency-Key": {"order-1"}, "X-Idempotency-Key": {"order-1"},
		}, body: true, wantHits: 1},
		{
			name:     "single key under ContextWithoutRetry",
			header:   http.Header{"Idempotency-Key": {"order-1"}},
			body:     true,
			ctx:      httpclient.ContextWithoutRetry,
			wantHits: 1,
		},
		{name: "bodyless duplicate key is resent by net/http", header: http.Header{"Idempotency-Key": {"order-1", "order-2"}}, wantHits: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var posts atomic.Int32

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					_, _ = io.WriteString(w, "warm")
					return
				}

				posts.Add(1)

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

			var reused atomic.Bool

			// The POST's first connection is the warmed one; a replay by
			// net/http dials a fresh one.
			trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
				if info.Reused {
					reused.Store(true)
				}
			}}

			warm, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
			require.NoError(t, err)

			resp, err := c.Do(warm)
			require.NoError(t, err)

			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()

			ctx := httptrace.WithClientTrace(t.Context(), trace)
			if tc.ctx != nil {
				ctx = tc.ctx(ctx)
			}

			var body io.Reader
			if tc.body {
				body = strings.NewReader("order")
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, body)
			require.NoError(t, err)

			req.Header = tc.header

			resp, err = c.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}

			require.Error(t, err)
			require.True(t, reused.Load(), "the POST must go over the warmed connection")
			require.Equal(t, tc.wantHits, posts.Load())
		})
	}
}
