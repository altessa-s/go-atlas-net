// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

// closingServer answers every request but /warm by reading it, counting it
// and dropping the connection before any reply.
func closingServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/warm" {
			_, _ = io.WriteString(w, "warm")
			return
		}

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

	return srv
}

// TestIdempotencyKey_ConnectionClosedBeforeReply sends a POST to a server that
// reads it and drops the connection before any reply: the request may have
// been processed. A POST with exactly one key is retried, an ambiguous key is
// refused before any transmission, and ContextWithoutRetry makes one attempt.
func TestIdempotencyKey_ConnectionClosedBeforeReply(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		header    http.Header
		ctx       func(context.Context) context.Context
		wantHits  int32
		ambiguous bool
	}{
		{name: "single key is retried", header: http.Header{"Idempotency-Key": {"order-1"}}, wantHits: 3},
		{name: "no key is sent once", header: http.Header{}, wantHits: 1},
		{
			name:     "single key under ContextWithoutRetry",
			header:   http.Header{"Idempotency-Key": {"order-1"}},
			ctx:      httpclient.ContextWithoutRetry,
			wantHits: 1,
		},
		{name: "duplicate key values", header: http.Header{"Idempotency-Key": {"order-1", "order-2"}}, ambiguous: true},
		{name: "both header names", header: http.Header{
			"Idempotency-Key": {"order-1"}, "X-Idempotency-Key": {"order-1"},
		}, ambiguous: true},
		{name: "blank key", header: http.Header{"Idempotency-Key": {"  "}}, ambiguous: true},
		{name: "present key without value", header: http.Header{"X-Idempotency-Key": {}}, ambiguous: true},
		{name: "non-canonical duplicate map key", header: http.Header{
			"Idempotency-Key": {"order-1"}, "idempotency-key": {"order-1"},
		}, ambiguous: true},
		{
			name:      "duplicate key under ContextWithoutRetry",
			header:    http.Header{"Idempotency-Key": {"order-1", "order-2"}},
			ctx:       httpclient.ContextWithoutRetry,
			ambiguous: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var hits atomic.Int32

			srv := closingServer(t, &hits)
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
			require.Equal(t, tc.ambiguous, errors.Is(err, httpclient.ErrAmbiguousIdempotencyKey), "error %v", err)
			require.Equal(t, tc.wantHits, hits.Load())
		})
	}
}

// TestIdempotencyKey_UnkeyedPostOnReusedConnection warms an HTTP/1.1
// connection, then sends an unkeyed POST over it that the server reads and
// answers by closing the connection: neither the retry layer nor net/http
// (which replays only idempotent methods and requests with a key header)
// resends it.
func TestIdempotencyKey_UnkeyedPostOnReusedConnection(t *testing.T) {
	t.Parallel()

	for name, withBody := range map[string]bool{"body": true, "bodyless": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var hits atomic.Int32

			srv := closingServer(t, &hits)
			c := httpclient.New(loopback, httpclient.WithRetryMax(2), httpclient.WithRetryWait(time.Millisecond, time.Millisecond))

			warm, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/warm", nil)
			require.NoError(t, err)

			resp, err := c.Do(warm)
			require.NoError(t, err)

			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()

			var reused atomic.Bool

			ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
				if info.Reused {
					reused.Store(true)
				}
			}})

			var body io.Reader
			if withBody {
				body = strings.NewReader("order")
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, body)
			require.NoError(t, err)

			resp, err = c.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}

			require.Error(t, err)
			require.True(t, reused.Load(), "the POST must go over the warmed connection")
			require.Equal(t, int32(1), hits.Load())
		})
	}
}

// TestIdempotencyKey_ProxyCallbackSeesCallerRequest checks that the request
// reaching the proxy callback carries the caller's header as sent and keeps
// GetBody, for a request sent once and for a retried one, and that a refused
// request never reaches it.
func TestIdempotencyKey_ProxyCallbackSeesCallerRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		header    http.Header
		ctx       func(context.Context) context.Context
		wantCalls int
	}{
		{name: "single key", header: http.Header{"Idempotency-Key": {"order-1"}, "X-Trace": {"a", "b"}}, wantCalls: 3},
		{name: "no key", header: http.Header{"X-Trace": {"a"}}, wantCalls: 1},
		{
			name:      "single key under ContextWithoutRetry",
			header:    http.Header{"idempotency-key": {"order-1"}},
			ctx:       httpclient.ContextWithoutRetry,
			wantCalls: 1,
		},
		{name: "ambiguous key", header: http.Header{"Idempotency-Key": {"order-1", "order-2"}}, wantCalls: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var hits atomic.Int32

			srv := closingServer(t, &hits)

			var (
				mu       sync.Mutex
				seen     []http.Header
				getBodys []bool
			)

			c := httpclient.New(loopback, httpclient.WithRetryMax(2), httpclient.WithRetryWait(time.Millisecond, time.Millisecond),
				httpclient.WithProxyFunc(func(r *http.Request) (*url.URL, error) {
					mu.Lock()
					defer mu.Unlock()

					seen = append(seen, r.Header.Clone())
					getBodys = append(getBodys, r.GetBody != nil)

					return nil, nil
				}))

			ctx := t.Context()
			if tc.ctx != nil {
				ctx = tc.ctx(ctx)
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, strings.NewReader("order"))
			require.NoError(t, err)

			req.Header = tc.header
			sent := tc.header.Clone()

			resp, err := c.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}

			require.Error(t, err)
			require.Equal(t, sent, req.Header, "the caller's header is not modified")

			mu.Lock()
			defer mu.Unlock()

			require.Len(t, seen, tc.wantCalls)

			for i := range seen {
				require.Equal(t, sent, seen[i])
				require.True(t, getBodys[i], "the request keeps GetBody")
			}

			require.Equal(t, int32(tc.wantCalls), hits.Load())
		})
	}
}
