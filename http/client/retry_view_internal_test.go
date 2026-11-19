// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/http/client/limiters"
	"github.com/altessa-s/go-atlas-net/httpx"
)

// countingCloser is a request body that counts its Close calls.
type countingCloser struct {
	io.Reader
	closes atomic.Int32
}

func (c *countingCloser) Close() error {
	c.closes.Add(1)
	return nil
}

// TestRetryRoundTripper_OptOutStillPreparesBody checks that ContextWithoutRetry
// caps the retry loop at one call but still hands an eligible request a
// replayable body, so the transport can rewind it.
func TestRetryRoundTripper_OptOutStillPreparesBody(t *testing.T) {
	t.Parallel()

	var (
		calls      int
		hasGetBody bool
	)

	rt := &retryRoundTripper{
		observer:          NoopObserver{},
		retryableStatuses: DefaultRetryableStatuses(),
		next: httpx.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			hasGetBody = req.GetBody != nil

			_, _ = io.Copy(io.Discard, req.Body)
			_ = req.Body.Close()

			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Request: req}, nil
		}),
		retryOpts:   newTestRetryOpts(2),
		maxAttempts: 2,
	}

	body := &countingCloser{Reader: strings.NewReader("payload")}

	req, err := http.NewRequestWithContext(ContextWithoutRetry(t.Context()), http.MethodPost, "http://example.com", body)
	require.NoError(t, err)
	require.Nil(t, req.GetBody, "the body has no GetBody of its own")

	req.Header.Set("Idempotency-Key", "k1")

	_, err = rt.RoundTrip(req) //nolint:bodyclose // an exhausted round trip returns no response
	require.Error(t, err)
	require.Equal(t, 1, calls)
	require.True(t, hasGetBody, "the transport can rewind the body")
	require.Equal(t, int32(1), body.closes.Load())
}

// newTrailerRequest returns a PUT whose rewindable streaming body sets its
// X-Sum trailer once read to its end.
func newTrailerRequest(t *testing.T) *http.Request {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "http://example.com", nil)
	require.NoError(t, err)

	req.Trailer = http.Header{"X-Sum": nil}
	req.Body = &trailerBody{r: strings.NewReader("payload"), trailer: req.Trailer}
	req.GetBody = func() (io.ReadCloser, error) {
		return &trailerBody{r: strings.NewReader("payload"), trailer: req.Trailer}, nil
	}

	return req
}

// TestRetryRoundTripper_PolicySeesDetachedView checks that a policy editing
// the request of an early reply (sent before the body was read) reaches
// neither the caller's trailers nor the next attempt.
func TestRetryRoundTripper_PolicySeesDetachedView(t *testing.T) {
	t.Parallel()

	var (
		calls          int
		callerAtSecond string
		sentTrailer    string
	)

	req := newTrailerRequest(t)

	rt := &retryRoundTripper{
		observer:          NoopObserver{},
		retryableStatuses: DefaultRetryableStatuses(),
		retryPolicyHandler: func(_ context.Context, resp *http.Response, _ error) (bool, error) {
			if resp.StatusCode != http.StatusServiceUnavailable {
				return false, nil
			}

			resp.Request.Trailer.Set("X-Sum", "evil")
			resp.Request.Header.Set("X-Extra", "evil")

			return true, nil
		},
		next: httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				// An early reply: the body is not read.
				_ = r.Body.Close()
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Request: r}, nil
			}

			callerAtSecond = req.Trailer.Get("X-Sum")

			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()

			sentTrailer = r.Trailer.Get("X-Sum")
			require.Empty(t, r.Header.Get("X-Extra"))

			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
		}),
		retryOpts:   newTestRetryOpts(2),
		maxAttempts: 2,
	}

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)

	_ = resp.Body.Close()

	require.Equal(t, 2, calls)
	require.Empty(t, callerAtSecond, "the policy's edit never reached the caller's trailers")
	require.Equal(t, "abc", sentTrailer)
}

// TestRetryRoundTripper_TransportRewindCapturesTrailers checks that a body the
// transport rewinds itself through GetBody is hooked too: once it is read to
// its end, later attempts send a copy of the captured trailers.
func TestRetryRoundTripper_TransportRewindCapturesTrailers(t *testing.T) {
	t.Parallel()

	var (
		calls      int
		sameAsLive bool
		sent       string
	)

	req := newTrailerRequest(t)

	rt := &retryRoundTripper{
		observer:          NoopObserver{},
		retryableStatuses: DefaultRetryableStatuses(),
		next: httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				// As net/http does on a reused connection: discard the body
				// and resend a rewound one, read to its end.
				_ = r.Body.Close()

				body, err := r.GetBody()
				require.NoError(t, err)

				_, _ = io.Copy(io.Discard, body)
				_ = body.Close()

				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Request: r}, nil
			}

			sameAsLive = reflect.ValueOf(r.Trailer).Pointer() == reflect.ValueOf(req.Trailer).Pointer()
			sent = r.Trailer.Get("X-Sum")

			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()

			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
		}),
		retryOpts:   newTestRetryOpts(2),
		maxAttempts: 2,
	}

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)

	_ = resp.Body.Close()

	require.Equal(t, 2, calls)
	require.False(t, sameAsLive, "the retry sends a copy of the captured trailers")
	require.Equal(t, "abc", sent)
}

// TestLimiterRefusal_ClosesBodyOnce checks that a limiter refusal closes the
// body it does not hand on, exactly once.
func TestLimiterRefusal_ClosesBodyOnce(t *testing.T) {
	t.Parallel()

	var invoked atomic.Bool

	lrt := limiters.NewRoundTripper(httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) {
		invoked.Store(true)
		return nil, errors.New("unreachable")
	}), limiters.Func(func(context.Context, string) error { return errors.New("denied") }))

	body := &countingCloser{Reader: strings.NewReader("x")}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.com", body)
	require.NoError(t, err)

	_, err = lrt.RoundTrip(req) //nolint:bodyclose // a refused round trip returns no response
	require.Error(t, err)
	require.False(t, invoked.Load())
	require.Equal(t, int32(1), body.closes.Load())
}

// TestBreakerRejection_ClosesBodyOnce checks that an open breaker closes the
// body of a request it rejects before delegation, exactly once.
func TestBreakerRejection_ClosesBodyOnce(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	transport := httpx.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)

		if req.Body != nil {
			_ = req.Body.Close()
		}

		return nil, errors.New("connection refused")
	})
	c := New(WithoutSSRFProtection(), WithClient(&http.Client{Transport: transport}), WithRetryMax(0),
		WithBreakerTimeout(time.Hour))

	for range DefaultBreakerMinRequests {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://down.test/", nil)
		require.NoError(t, err)

		_, err = c.Do(req) //nolint:bodyclose // a failed request returns no response
		require.Error(t, err)
	}

	before := calls.Load()
	body := &countingCloser{Reader: strings.NewReader("x")}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://down.test/", body)
	require.NoError(t, err)

	_, err = c.Do(req) //nolint:bodyclose // a rejected request returns no response
	require.True(t, IsCircuitBreakerOpen(err), "error %v", err)
	require.Equal(t, before, calls.Load(), "the transport is not invoked")
	require.Equal(t, int32(1), body.closes.Load())
}

// TestRetryRoundTripper_NilURLChange checks that a caller setting its
// request's URL to nil during the loop stops it without a panic, and that the
// outcome is reported for the snapshot.
func TestRetryRoundTripper_NilURLChange(t *testing.T) {
	t.Parallel()

	var (
		calls int
		hosts []string
	)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/path", nil)
	require.NoError(t, err)

	rt := &retryRoundTripper{
		observer:          hostRecorder{hosts: &hosts},
		retryableStatuses: DefaultRetryableStatuses(),
		retryPolicyHandler: func(context.Context, *http.Response, error) (bool, error) {
			req.URL = nil
			return true, nil
		},
		next: httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Request: r}, nil
		}),
		retryOpts:   newTestRetryOpts(2),
		maxAttempts: 2,
	}

	_, err = rt.RoundTrip(req) //nolint:bodyclose // a failed round trip returns no response
	require.ErrorIs(t, err, errRequestChanged)
	require.Equal(t, 1, calls)
	require.Equal(t, []string{"example.com"}, hosts)
}

// hostRecorder records the host of every RequestDone.
type hostRecorder struct {
	NoopObserver

	hosts *[]string
}

func (h hostRecorder) RequestDone(e RequestEvent) { *h.hosts = append(*h.hosts, e.Host) }

// TestRetryRoundTripper_ZeroRetriesLeaveBodyAsIs checks the exception to body
// preparation: with no retries configured (WithRetryMax(0), WithoutRetry)
// an eligible body without GetBody is not buffered but handed on as is.
func TestRetryRoundTripper_ZeroRetriesLeaveBodyAsIs(t *testing.T) {
	t.Parallel()

	var (
		calls      int
		hasGetBody bool
		sameBody   bool
	)

	body := &countingCloser{Reader: strings.NewReader("payload")}

	rt := &retryRoundTripper{
		observer:          NoopObserver{},
		retryableStatuses: DefaultRetryableStatuses(),
		next: httpx.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			hasGetBody = req.GetBody != nil
			sameBody = req.Body == io.ReadCloser(body)

			_ = req.Body.Close()

			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Request: req}, nil
		}),
		retryOpts:   newTestRetryOpts(0),
		maxAttempts: 0,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.com", body)
	require.NoError(t, err)

	req.Header.Set("Idempotency-Key", "k1")

	_, err = rt.RoundTrip(req) //nolint:bodyclose // an exhausted round trip returns no response
	require.Error(t, err)
	require.Equal(t, 1, calls)
	require.False(t, hasGetBody)
	require.True(t, sameBody, "the caller's body is handed on unbuffered")
	require.Equal(t, int32(1), body.closes.Load())
}
