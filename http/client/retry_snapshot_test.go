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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/http/client/limiters"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

// recordedRequest is what a test server saw of one request.
type recordedRequest struct {
	key              []string
	body             string
	contentLength    int64
	transferEncoding []string
}

// recordingServer answers every request with 503 and records it.
func recordingServer(t *testing.T) (*httptest.Server, func() []recordedRequest) {
	t.Helper()

	var (
		mu   sync.Mutex
		seen []recordedRequest
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)

		mu.Lock()

		seen = append(seen, recordedRequest{
			key:              r.Header.Values("Idempotency-Key"),
			body:             string(b),
			contentLength:    r.ContentLength,
			transferEncoding: r.TransferEncoding,
		})
		mu.Unlock()

		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	return srv, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()

		return slicesClone(seen)
	}
}

func slicesClone[T any](s []T) []T { return append([]T(nil), s...) }

// fastRetry is a client configuration with two quick retries.
func fastRetry(opts ...httpclient.Option) *http.Client {
	return httpclient.New(append([]httpclient.Option{
		loopback, httpclient.WithRetryMax(2), httpclient.WithRetryWait(time.Millisecond, time.Millisecond),
	}, opts...)...)
}

// TestRetrySnapshot_PolicyCannotChangeResentRequest checks that every attempt
// is built from the request as it was before the first transmission: a policy
// editing the attempt it sees (resp.Request) changes nothing that is resent.
func TestRetrySnapshot_PolicyCannotChangeResentRequest(t *testing.T) {
	t.Parallel()

	srv, seen := recordingServer(t)
	c := fastRetry(httpclient.WithRetryPolicyHandler(func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		if resp != nil && resp.Request != nil {
			resp.Request.Header.Set("Idempotency-Key", "B")
			resp.Request.Method = http.MethodDelete
		}

		return httpclient.DefaultRetryPolicy(ctx, resp, err)
	}))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("order"))
	require.NoError(t, err)
	req.Header.Set("Idempotency-Key", "A")

	_, err = c.Do(req) //nolint:bodyclose // an exhausted request returns no response
	require.ErrorIs(t, err, httpclient.ErrMaxRetriesExceeded)

	got := seen()
	require.Len(t, got, 3)

	for _, r := range got {
		require.Equal(t, []string{"A"}, r.key, "key B never reaches the server")
		require.Equal(t, "order", r.body)
	}
}

// TestRetrySnapshot_CallerChangeStopsReplay checks that a change to the
// caller's request during the retry loop stops it before the next attempt:
// neither the old nor the edited request is resent.
func TestRetrySnapshot_CallerChangeStopsReplay(t *testing.T) {
	t.Parallel()

	srv, seen := recordingServer(t)

	var req *http.Request

	c := fastRetry(httpclient.WithRetryPolicyHandler(func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		req.Header.Set("Idempotency-Key", "B")
		return httpclient.DefaultRetryPolicy(ctx, resp, err)
	}))

	var err error

	req, err = http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("order"))
	require.NoError(t, err)
	req.Header.Set("Idempotency-Key", "A")

	_, err = c.Do(req) //nolint:bodyclose // a failed request returns no response
	require.ErrorContains(t, err, "request changed during the retry loop")
	require.Len(t, seen(), 1)
}

// TestRetrySnapshot_KeepsFramingAndCallerFields checks that buffering a body
// for replay is private: every attempt of an unknown-length body is sent
// chunked, as the first, and the caller's request keeps its fields.
func TestRetrySnapshot_KeepsFramingAndCallerFields(t *testing.T) {
	t.Parallel()

	srv, seen := recordingServer(t)
	c := fastRetry()

	// Hide the reader type so net/http knows neither the length nor GetBody.
	body := &closeRecorder{Reader: strings.NewReader("order")}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, body)
	require.NoError(t, err)

	req.Header["idempotency-key"] = []string{"k1"}

	_, err = c.Do(req) //nolint:bodyclose // an exhausted request returns no response
	require.ErrorIs(t, err, httpclient.ErrMaxRetriesExceeded)

	got := seen()
	require.Len(t, got, 3, "a single lower-case key makes the POST replayable")

	for _, r := range got {
		require.Equal(t, "order", r.body)
		require.Equal(t, int64(-1), r.contentLength)
		require.Equal(t, []string{"chunked"}, r.transferEncoding)
	}

	require.Same(t, body, req.Body, "the caller's Body is not replaced")
	require.True(t, body.closed.Load(), "the caller's body is closed once buffered")
	require.Nil(t, req.GetBody, "the caller's GetBody is not set")
	require.Zero(t, req.ContentLength, "the caller's ContentLength is not changed")
	require.Equal(t, http.Header{"idempotency-key": {"k1"}}, req.Header)
}

// TestAmbiguousIdempotencyKey_BeforeLimiter checks that an ambiguous key is
// refused before the limiter, with the body closed and zero attempts.
func TestAmbiguousIdempotencyKey_BeforeLimiter(t *testing.T) {
	t.Parallel()

	for name, perAttempt := range map[string]bool{"whole request": false, "per attempt": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var limiterCalls atomic.Int32

			obs := &recordingObserver{}
			opts := []httpclient.Option{
				httpclient.WithObserver(obs),
				httpclient.WithLimiter(limiters.Func(func(context.Context, string) error {
					limiterCalls.Add(1)
					return errors.New("denied")
				})),
			}

			if perAttempt {
				opts = append(opts, httpclient.WithLimiterPerAttempt())
			}

			c := fastRetry(opts...)

			body := &closeRecorder{Reader: strings.NewReader("order")}

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1:1/", body)
			require.NoError(t, err)

			req.Header["Idempotency-Key"] = []string{"a", "b"}

			_, err = c.Do(req) //nolint:bodyclose // a refused request returns no response
			require.ErrorIs(t, err, httpclient.ErrAmbiguousIdempotencyKey)
			require.Zero(t, limiterCalls.Load())
			require.True(t, body.closed.Load())

			obs.mu.Lock()
			defer obs.mu.Unlock()

			require.Len(t, obs.done, 1)
			require.Zero(t, obs.done[0].Retries)
			require.True(t, obs.done[0].Failed)
		})
	}
}

// closeRecorder is a request body that records being closed.
type closeRecorder struct {
	io.Reader
	closed atomic.Bool
}

func (c *closeRecorder) Close() error {
	c.closed.Store(true)
	return nil
}

// TestAmbiguousIdempotencyKey_SurvivesErrorHandler checks that the refusal
// is terminal: an ErrorHandler that would turn errors into replies is not
// consulted.
func TestAmbiguousIdempotencyKey_SurvivesErrorHandler(t *testing.T) {
	t.Parallel()

	var handlerCalls atomic.Int32

	c := fastRetry(httpclient.WithErrorHandler(func(*http.Response, error, int) (*http.Response, error) {
		handlerCalls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	}))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPatch, "http://127.0.0.1:1/", strings.NewReader("x"))
	require.NoError(t, err)

	req.Header.Set("X-Idempotency-Key", " ")

	resp, err := c.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}

	require.ErrorIs(t, err, httpclient.ErrAmbiguousIdempotencyKey)
	require.Nil(t, resp)
	require.Zero(t, handlerCalls.Load())
}

// TestBuilder_StoredContextWithoutRetry checks that the no-argument builder
// methods use the context set with Context, so ContextWithoutRetry applies.
func TestBuilder_StoredContextWithoutRetry(t *testing.T) {
	t.Parallel()

	srv, seen := recordingServer(t)
	c := fastRetry()

	resp, err := httpclient.NewRequestBuilder(c).GET(srv.URL).
		Context(httpclient.ContextWithoutRetry(t.Context())).SendWithoutContext()
	if resp != nil {
		_ = resp.Body.Close()
	}

	require.Error(t, err)
	require.Len(t, seen(), 1, "one call: the stored context opts out of retries")

	rb := httpclient.NewRequestBuilder(c).GET(srv.URL).Context(httpclient.ContextWithoutRetry(t.Context()))
	t.Cleanup(rb.Release)

	req, err := rb.Build()
	require.NoError(t, err)
	require.True(t, httpclient.RetryDisabled(req.Context()))

	unset, err := httpclient.NewRequestBuilder(c).GET(srv.URL).Build()
	require.NoError(t, err)
	require.False(t, httpclient.RetryDisabled(unset.Context()))
}
