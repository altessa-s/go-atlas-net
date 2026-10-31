// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package limiters

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/httpx"
)

type mockLimiter struct {
	err error
}

func (m *mockLimiter) Allow(_ context.Context, _ string) error {
	return m.err
}

func TestNewRoundTripper(t *testing.T) {
	rt := NewRoundTripper(http.DefaultTransport, &mockLimiter{})
	require.NotNil(t, rt)
}

func TestNewRoundTripper_NilNext(t *testing.T) {
	rt := NewRoundTripper(nil, &mockLimiter{})
	require.NotNil(t, rt)
	// next should default to http.DefaultTransport
	require.NotNil(t, rt.next)
}

func TestRoundTripper_RoundTrip_Allowed(t *testing.T) {
	next := httpx.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK}, nil
	})
	rt := NewRoundTripper(next, &mockLimiter{})

	req, _ := http.NewRequest("GET", "http://example.com/test", nil)
	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestRoundTripper_RoundTrip_Denied(t *testing.T) {
	rt := NewRoundTripper(http.DefaultTransport, &mockLimiter{err: errors.New("rate limited")})

	req, _ := http.NewRequest("GET", "http://example.com/test", nil)
	_, err := rt.RoundTrip(req)
	require.Error(t, err)
}

func TestRoundTripper_RoundTrip_HostnameExtracted(t *testing.T) {
	var capturedKey string
	limiter := &capturingLimiter{capturedKey: &capturedKey}
	next := httpx.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK}, nil
	})
	rt := NewRoundTripper(next, limiter)

	req, _ := http.NewRequest("GET", "http://api.example.com:8080/test", nil)
	_, err := rt.RoundTrip(req)
	require.NoError(t, err)
	require.Equal(t, "api.example.com", capturedKey)
}

func TestRoundTripper_RoundTrip_NilLimiterPassesThrough(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		limiter RequestsLimiter
	}{
		{name: "nil", limiter: nil},
		{name: "empty chain", limiter: Chain()},
		{name: "typed nil", limiter: (*mockLimiter)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			next := httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: http.StatusOK}, nil
			})
			rt := NewRoundTripper(next, tc.limiter)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
			require.NoError(t, err)
			resp, err := rt.RoundTrip(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, 1, calls)
		})
	}
}

type capturingLimiter struct {
	capturedKey *string
}

func (l *capturingLimiter) Allow(_ context.Context, key string) error {
	*l.capturedKey = key
	return nil
}
