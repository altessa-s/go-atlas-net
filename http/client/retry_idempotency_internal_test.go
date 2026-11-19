// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/httpx"
)

// countingRetryRT returns a retry round tripper that answers every attempt
// with 503 and counts the attempts.
func countingRetryRT(t *testing.T, attempts *int) *retryRoundTripper {
	t.Helper()

	return &retryRoundTripper{
		observer:          NoopObserver{},
		retryableStatuses: DefaultRetryableStatuses(),
		next: httpx.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if req.Body != nil {
				_, _ = io.Copy(io.Discard, req.Body)
				_ = req.Body.Close()
			}

			*attempts++

			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Request: req}, nil
		}),
		retryOpts:   newTestRetryOpts(2),
		maxAttempts: 2,
	}
}

func TestRetryRoundTripper_IdempotencyKeyRule(t *testing.T) {
	t.Parallel()

	// attempts 0: the request is refused with ErrAmbiguousIdempotencyKey.
	tests := []struct {
		name     string
		method   string
		header   http.Header
		attempts int
	}{
		{name: "empty method is GET", method: "", attempts: 3},
		{name: "OPTIONS", method: http.MethodOptions, attempts: 3},
		{name: "TRACE", method: http.MethodTrace, attempts: 3},
		{name: "HEAD", method: http.MethodHead, attempts: 3},
		{name: "single key", method: http.MethodPost, header: http.Header{"Idempotency-Key": {"k1"}}, attempts: 3},
		{name: "single X key", method: http.MethodPatch, header: http.Header{"X-Idempotency-Key": {"k1"}}, attempts: 3},
		{name: "single key with surrounding whitespace", method: http.MethodPost, header: http.Header{"Idempotency-Key": {" \tk1\t "}}, attempts: 3},
		{name: "single non-canonical key", method: http.MethodPost, header: http.Header{"idempotency-key": {"k1"}}, attempts: 3},
		{name: "no key", method: http.MethodPost, attempts: 1},
		{name: "duplicate values", method: http.MethodPost, header: http.Header{"Idempotency-Key": {"k1", "k2"}}, attempts: 0},
		{name: "same value twice", method: http.MethodPost, header: http.Header{"Idempotency-Key": {"k1", "k1"}}, attempts: 0},
		{name: "both header names", method: http.MethodPost, header: http.Header{
			"Idempotency-Key": {"k1"}, "X-Idempotency-Key": {"k1"},
		}, attempts: 0},
		{name: "blank key", method: http.MethodPost, header: http.Header{"Idempotency-Key": {""}}, attempts: 0},
		{name: "whitespace key", method: http.MethodPost, header: http.Header{"Idempotency-Key": {" \t "}}, attempts: 0},
		{name: "blank first value", method: http.MethodPost, header: http.Header{"Idempotency-Key": {"", "k1"}}, attempts: 0},
		{name: "empty value list", method: http.MethodPost, header: http.Header{"Idempotency-Key": {}}, attempts: 0},
		{name: "canonical and lowercase map keys", method: http.MethodPost, header: http.Header{
			"Idempotency-Key": {"k1"}, "idempotency-key": {"k2"},
		}, attempts: 0},
		{name: "lowercase X map key besides canonical key", method: http.MethodPost, header: http.Header{
			"Idempotency-Key": {"k1"}, "x-idempotency-key": {"k1"},
		}, attempts: 0},
		{name: "lowercase post", method: "post", attempts: 1},
		{name: "lowercase put", method: "put", attempts: 1},
		{name: "lowercase get", method: "get", attempts: 1},
		{name: "lowercase post with single key", method: "post", header: http.Header{"Idempotency-Key": {"k1"}}, attempts: 3},
		{name: "lowercase post with duplicate key", method: "post", header: http.Header{"Idempotency-Key": {"k1", "k2"}}, attempts: 0},
		{name: "GET with duplicate key", method: http.MethodGet, header: http.Header{"Idempotency-Key": {"k1", "k2"}}, attempts: 3},
		{name: "DELETE with blank key", method: http.MethodDelete, header: http.Header{"Idempotency-Key": {""}}, attempts: 3},
		{name: "Kelvin sign in header name", method: http.MethodPost, header: http.Header{"Idempotency-\u212Aey": {"k1"}}, attempts: 1},
		{name: "Kelvin sign in X header name", method: http.MethodPost, header: http.Header{"X-Idempotency-\u212Aey": {"k1"}}, attempts: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var attempts int

			rt := countingRetryRT(t, &attempts)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com", strings.NewReader("payload"))
			require.NoError(t, err)

			req.Method = tc.method
			if tc.header != nil {
				req.Header = tc.header
			}

			body := &closeTracker{Reader: strings.NewReader("payload")}
			req.Body = body

			_, err = rt.RoundTrip(req) //nolint:bodyclose // a failed round trip returns no response
			if tc.attempts == 0 {
				require.ErrorIs(t, err, ErrAmbiguousIdempotencyKey)
				require.Zero(t, attempts, "refused before any transmission")
				require.True(t, body.closed, "the refused request's body is closed")

				return
			}

			exhaustedErr, ok := errors.AsType[*RetryExhaustedError](err)
			require.True(t, ok, "error %v", err)
			require.Equal(t, tc.attempts, exhaustedErr.Attempts)
			require.Equal(t, tc.attempts, attempts)
		})
	}
}

// closeTracker is a request body that records being closed.
type closeTracker struct {
	io.Reader
	closed bool
}

func (c *closeTracker) Close() error {
	c.closed = true
	return nil
}

func TestIdempotencyKey_DoesNotMutateHeader(t *testing.T) {
	t.Parallel()

	key := make([]string, 1, 4)
	key[0] = "k1"
	xkey := make([]string, 1, 4)
	xkey[0] = "k2"

	h := http.Header{"Idempotency-Key": key, "x-idempotency-key": xkey}
	present, single := idempotencyKey(h)
	require.True(t, present)
	require.False(t, single)
	require.Equal(t, http.Header{"Idempotency-Key": {"k1"}, "x-idempotency-key": {"k2"}}, h)
	require.Equal(t, []string{"k1", "", "", ""}, key[:cap(key)], "spare capacity of the caller's slice is untouched")
	require.Equal(t, []string{"k2", "", "", ""}, xkey[:cap(xkey)], "spare capacity of the caller's slice is untouched")

	one := http.Header{"Idempotency-Key": key}
	present, ok := idempotencyKey(one)
	require.True(t, present)
	require.True(t, ok)
	require.Equal(t, []string{"k1", "", "", ""}, key[:cap(key)])
}

func TestRetryRoundTripper_ContextWithoutRetry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		header http.Header
		policy RetryPolicyHandler
	}{
		{name: "GET", method: http.MethodGet},
		{name: "POST with single key", method: http.MethodPost, header: http.Header{"Idempotency-Key": {"k1"}}},
		{name: "policy asks to retry", method: http.MethodGet, policy: func(context.Context, *http.Response, error) (bool, error) {
			return true, nil
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var attempts int

			rt := countingRetryRT(t, &attempts)
			rt.retryPolicyHandler = tc.policy

			ctx := ContextWithoutRetry(t.Context())
			require.True(t, RetryDisabled(ctx))
			require.False(t, RetryDisabled(t.Context()))

			req, err := http.NewRequestWithContext(ctx, tc.method, "http://example.com", strings.NewReader("payload"))
			require.NoError(t, err)

			if tc.header != nil {
				req.Header = tc.header
			}

			_, err = rt.RoundTrip(req) //nolint:bodyclose // an exhausted round trip returns no response
			exhaustedErr, ok := errors.AsType[*RetryExhaustedError](err)
			require.True(t, ok, "error %v", err)
			require.Equal(t, 1, exhaustedErr.Attempts)
			require.Equal(t, 1, attempts)
		})
	}
}

// TestRetryRoundTripper_PolicyCannotReplayIneligible checks that a policy
// asking to retry everything cannot replay a request the rule sends once nor
// let an ambiguous key through, and that the request reaches the next round
// tripper unmodified.
func TestRetryRoundTripper_PolicyCannotReplayIneligible(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		header   http.Header
		ctx      func(context.Context) context.Context
		attempts int
	}{
		{name: "duplicate key is refused", header: http.Header{"Idempotency-Key": {"k1", "k2"}}, attempts: 0},
		{name: "no key", header: http.Header{}, attempts: 1},
		{name: "single key under ContextWithoutRetry", header: http.Header{"Idempotency-Key": {"k1"}}, ctx: ContextWithoutRetry, attempts: 1},
		{name: "single key", header: http.Header{"Idempotency-Key": {"k1"}}, attempts: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var (
				calls      int
				policyRuns int
				getBodies  []bool
				headers    []http.Header
			)

			rt := &retryRoundTripper{
				observer:          NoopObserver{},
				retryableStatuses: DefaultRetryableStatuses(),
				retryPolicyHandler: func(context.Context, *http.Response, error) (bool, error) {
					policyRuns++
					return true, nil
				},
				next: httpx.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
					calls++

					getBodies = append(getBodies, req.GetBody != nil)
					headers = append(headers, req.Header)
					_, _ = io.Copy(io.Discard, req.Body)
					_ = req.Body.Close()

					return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Request: req}, nil
				}),
				retryOpts:   newTestRetryOpts(2),
				maxAttempts: 2,
			}

			ctx := t.Context()
			if tc.ctx != nil {
				ctx = tc.ctx(ctx)
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://example.com", strings.NewReader("payload"))
			require.NoError(t, err)

			req.Header = tc.header

			sent := req.Header.Clone()

			_, err = rt.RoundTrip(req) //nolint:bodyclose // a failed round trip returns no response
			require.Error(t, err)

			if tc.attempts == 0 {
				require.ErrorIs(t, err, ErrAmbiguousIdempotencyKey)
			}

			require.Equal(t, tc.attempts, calls)
			require.Equal(t, tc.attempts, policyRuns, "the policy classifies every attempt")

			for i := range calls {
				require.True(t, getBodies[i], "the attempt keeps GetBody")
				require.Equal(t, sent, headers[i], "the attempt's header is the caller's")
			}
		})
	}
}
