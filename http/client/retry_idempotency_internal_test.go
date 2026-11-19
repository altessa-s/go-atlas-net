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
		{name: "duplicate values", method: http.MethodPost, header: http.Header{"Idempotency-Key": {"k1", "k2"}}, attempts: 1},
		{name: "same value twice", method: http.MethodPost, header: http.Header{"Idempotency-Key": {"k1", "k1"}}, attempts: 1},
		{name: "both header names", method: http.MethodPost, header: http.Header{
			"Idempotency-Key": {"k1"}, "X-Idempotency-Key": {"k1"},
		}, attempts: 1},
		{name: "blank key", method: http.MethodPost, header: http.Header{"Idempotency-Key": {""}}, attempts: 1},
		{name: "whitespace key", method: http.MethodPost, header: http.Header{"Idempotency-Key": {" \t "}}, attempts: 1},
		{name: "blank first value", method: http.MethodPost, header: http.Header{"Idempotency-Key": {"", "k1"}}, attempts: 1},
		{name: "empty value list", method: http.MethodPost, header: http.Header{"Idempotency-Key": {}}, attempts: 1},
		{name: "canonical and lowercase map keys", method: http.MethodPost, header: http.Header{
			"Idempotency-Key": {"k1"}, "idempotency-key": {"k2"},
		}, attempts: 1},
		{name: "lowercase X map key besides canonical key", method: http.MethodPost, header: http.Header{
			"Idempotency-Key": {"k1"}, "x-idempotency-key": {"k1"},
		}, attempts: 1},
		{name: "lowercase post", method: "post", attempts: 1},
		{name: "lowercase put", method: "put", attempts: 1},
		{name: "lowercase get", method: "get", attempts: 1},
		{name: "lowercase post with single key", method: "post", header: http.Header{"Idempotency-Key": {"k1"}}, attempts: 3},
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

			_, err = rt.RoundTrip(req) //nolint:bodyclose // an exhausted round trip returns no response
			exhaustedErr, ok := errors.AsType[*RetryExhaustedError](err)
			require.True(t, ok, "error %v", err)
			require.Equal(t, tc.attempts, exhaustedErr.Attempts)
			require.Equal(t, tc.attempts, attempts)
		})
	}
}

func TestSingleIdempotencyKey_DoesNotMutateHeader(t *testing.T) {
	t.Parallel()

	key := make([]string, 1, 4)
	key[0] = "k1"
	xkey := make([]string, 1, 4)
	xkey[0] = "k2"

	h := http.Header{"Idempotency-Key": key, "x-idempotency-key": xkey}
	require.False(t, singleIdempotencyKey(h))
	require.Equal(t, http.Header{"Idempotency-Key": {"k1"}, "x-idempotency-key": {"k2"}}, h)
	require.Equal(t, []string{"k1", "", "", ""}, key[:cap(key)], "spare capacity of the caller's slice is untouched")
	require.Equal(t, []string{"k2", "", "", ""}, xkey[:cap(xkey)], "spare capacity of the caller's slice is untouched")

	single := http.Header{"Idempotency-Key": key}
	require.True(t, singleIdempotencyKey(single))
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
