// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client_test

import (
	"context"
	"iter"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

// pairs returns an iterator over the given key/value pairs, in order.
func pairs(kv ...[2]string) iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		for _, p := range kv {
			if !yield(p[0], p[1]) {
				return
			}
		}
	}
}

// TestWithHeaders_CaseVariants checks WithHeaders through PostWithOptions in
// both iteration orders: an ordinary header ends with one value, the same in
// both orders; idempotency key variants are all kept and refused.
func TestWithHeaders_CaseVariants(t *testing.T) {
	t.Parallel()

	var (
		mu           sync.Mutex
		contentTypes [][]string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()

		contentTypes = append(contentTypes, r.Header.Values("Content-Type"))
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := httpclient.NewHTTPClient(loopback)

	ct := [][2]string{{"Content-Type", "application/a"}, {"content-type", "application/b"}}
	keys := [][2]string{{"Idempotency-Key", "A"}, {"idempotency-key", "B"}}

	for _, order := range [][2]int{{0, 1}, {1, 0}} {
		resp, err := c.PostWithOptions(t.Context(), srv.URL, "text/plain", strings.NewReader("x"),
			httpclient.WithHeaders(pairs(ct[order[0]], ct[order[1]])))
		require.NoError(t, err)

		_ = resp.Body.Close()

		_, err = c.PostWithOptions(t.Context(), srv.URL, "text/plain", strings.NewReader("x"), //nolint:bodyclose // refused
			httpclient.WithHeaders(pairs(keys[order[0]], keys[order[1]])))
		require.ErrorIs(t, err, httpclient.ErrAmbiguousIdempotencyKey)
	}

	mu.Lock()
	defer mu.Unlock()

	require.Equal(t, [][]string{{"application/b"}, {"application/b"}}, contentTypes,
		"one Content-Type, the same in both orders; the refused requests never arrive")
}

// TestRetrySnapshot_URLChangeDetection checks that the snapshot is what
// telemetry reports and that the live URL is compared structurally: a Host
// change of an Opaque URL, which URL.String omits, stops the loop.
func TestRetrySnapshot_URLChangeDetection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		opaque bool
		change func(*http.Request)
	}{
		{name: "host of an opaque URL", opaque: true, change: func(r *http.Request) { r.URL.Host = "other.test:1" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv, seen := recordingServer(t)
			obs := &recordingObserver{}

			u, err := url.Parse(srv.URL)
			require.NoError(t, err)

			host := u.Hostname()

			var req *http.Request

			c := fastRetry(httpclient.WithObserver(obs),
				httpclient.WithRetryPolicyHandler(func(ctx context.Context, resp *http.Response, err error) (bool, error) {
					tc.change(req)
					return httpclient.DefaultRetryPolicy(ctx, resp, err)
				}))

			req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/path", nil)
			require.NoError(t, err)

			if tc.opaque {
				req.URL.Opaque = "/path"
			}

			_, err = c.Do(req) //nolint:bodyclose // a failed request returns no response
			require.ErrorContains(t, err, "request changed during the retry loop")
			require.Len(t, seen(), 1)

			obs.mu.Lock()
			defer obs.mu.Unlock()

			require.Len(t, obs.done, 1)
			require.Equal(t, host, obs.done[0].Host, "telemetry reports the snapshot")
		})
	}
}
