// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-core/time/clock"

	"github.com/altessa-s/go-atlas-net/httpx"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

// countingBody is a request body that counts its Close calls. Its reads run
// onRead first, when set, and fail with readErr, when set.
type countingBody struct {
	r       io.Reader
	closed  atomic.Int32
	onRead  func()
	readErr error
}

func (b *countingBody) Read(p []byte) (int, error) {
	if b.onRead != nil {
		b.onRead()
	}
	if b.readErr != nil {
		return 0, b.readErr
	}
	return b.r.Read(p)
}

func (b *countingBody) Close() error {
	b.closed.Add(1)
	return nil
}

// unreachable is a transport the test expects never to be called.
func unreachable(t *testing.T) http.RoundTripper {
	t.Helper()
	return httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Error("no attempt may reach the transport")
		return nil, errors.New("unreachable")
	})
}

// TestRetry_ClosesUnsentBodyOnEndedContext checks that a request whose
// context ended before the first attempt has its body closed, as
// http.RoundTripper requires, whether the retry layer buffered it or not.
func TestRetry_ClosesUnsentBodyOnEndedContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		method  string
		opt     httpclient.Option
		getBody bool
	}{
		{name: "without retry", method: http.MethodPut, opt: httpclient.WithoutRetry()},
		{name: "not replayable", method: http.MethodPost, opt: httpclient.WithRetryMax(3)},
		{name: "caller replay source", method: http.MethodPut, opt: httpclient.WithRetryMax(3), getBody: true},
		{name: "buffered", method: http.MethodPut, opt: httpclient.WithRetryMax(3)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := httpclient.New(httpclient.WithoutSSRFProtection(), httpclient.WithClient(&http.Client{Transport: unreachable(t)}), tt.opt)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			body := &countingBody{r: strings.NewReader("payload")}
			req, err := http.NewRequestWithContext(ctx, tt.method, "http://api.test/x", body)
			require.NoError(t, err)
			if tt.getBody {
				req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("payload")), nil }
			}

			resp, err := c.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			require.ErrorIs(t, err, context.Canceled)
			require.EqualValues(t, 1, body.closed.Load(), "the unsent request body is closed once")
		})
	}
}

// TestRetry_ErrorHandlerCountsAttemptsMade checks that the ErrorHandler
// receives the number of attempts actually sent, whatever ended the loop.
func TestRetry_ErrorHandlerCountsAttemptsMade(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		status int
		opts   []httpclient.Option
		// cancelOn cancels the request context during the given call; a
		// negative value cancels it before the request is sent.
		cancelOn int32
		want     int
	}{
		{name: "permanent status", method: http.MethodGet, status: http.StatusBadRequest, opts: []httpclient.Option{httpclient.WithRetryMax(5)}, want: 1},
		{name: "not replayable", method: http.MethodPost, status: http.StatusServiceUnavailable, opts: []httpclient.Option{httpclient.WithRetryMax(5)}, want: 1},
		{name: "ended before sending", method: http.MethodGet, status: http.StatusServiceUnavailable, opts: []httpclient.Option{httpclient.WithRetryMax(5)}, cancelOn: -1},
		{name: "ended during backoff", method: http.MethodGet, status: http.StatusServiceUnavailable, opts: []httpclient.Option{httpclient.WithRetryMax(5)}, cancelOn: 2, want: 2},
		{
			name:   "elapsed budget",
			method: http.MethodGet,
			status: http.StatusServiceUnavailable,
			opts: []httpclient.Option{
				httpclient.WithRetryMax(5), httpclient.WithRetryMaxElapsed(10 * time.Millisecond), httpclient.WithRetryWait(time.Second, time.Second),
			},
			want: 1,
		},
		{name: "exhausted", method: http.MethodGet, status: http.StatusServiceUnavailable, opts: []httpclient.Option{httpclient.WithRetryMax(2)}, want: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.cancelOn < 0 {
				cancel()
			}
			var calls atomic.Int32
			transport := httpx.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if calls.Add(1) == tt.cancelOn {
					cancel()
				}
				return &http.Response{StatusCode: tt.status, Body: http.NoBody, Request: req}, nil
			})
			got := -1
			handler := func(_ *http.Response, err error, numTries int) (*http.Response, error) {
				got = numTries
				return nil, err
			}
			opts := append([]httpclient.Option{
				httpclient.WithoutSSRFProtection(),
				httpclient.WithClient(&http.Client{Transport: transport}),
				httpclient.WithErrorHandler(handler),
				httpclient.WithRetryWait(5*time.Millisecond, 5*time.Millisecond),
			}, tt.opts...)
			c := httpclient.New(opts...)

			req, err := http.NewRequestWithContext(ctx, tt.method, "http://api.test/x", nil)
			require.NoError(t, err)
			resp, err := c.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			require.Error(t, err)
			require.EqualValues(t, tt.want, calls.Load(), "attempts sent")
			require.Equal(t, tt.want, got, "numTries counts the attempts sent")
		})
	}
}

// TestObserver_BufferingFailure checks that a replayable request whose body
// fails while being buffered is reported as one failed request, timed from
// the start of the round trip.
func TestObserver_BufferingFailure(t *testing.T) {
	t.Parallel()

	fake := clock.NewFake(time.Now())
	obs := &recordingObserver{}
	c := httpclient.New(
		httpclient.WithoutSSRFProtection(),
		httpclient.WithClient(&http.Client{Transport: unreachable(t)}),
		httpclient.WithObserver(obs),
		httpclient.WithClock(fake),
		httpclient.WithRetryMax(3),
	)
	readErr := errors.New("disk read failed")
	body := &countingBody{onRead: func() { fake.Advance(2 * time.Second) }, readErr: readErr}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "http://api.test/x", body)
	require.NoError(t, err)

	resp, err := c.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.ErrorIs(t, err, readErr)
	require.EqualValues(t, 1, body.closed.Load())

	obs.mu.Lock()
	defer obs.mu.Unlock()
	require.Equal(t, []httpclient.RequestEvent{{
		Method:      http.MethodPut,
		Host:        "api.test",
		StatusClass: "unknown",
		Failed:      true,
		Duration:    2 * time.Second,
	}}, obs.done, "one failed completion, timed with the buffering")
}

// pipeBody is a request body reading from a pipe whose writer the test keeps
// open: a read blocks until the body is closed. It counts its Close calls.
type pipeBody struct {
	r      *io.PipeReader
	reads  chan struct{}
	closed atomic.Int32
}

func newPipeBody(t *testing.T) *pipeBody {
	t.Helper()
	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })
	return &pipeBody{r: r, reads: make(chan struct{}, 1)}
}

func (b *pipeBody) Read(p []byte) (int, error) {
	select {
	case b.reads <- struct{}{}:
	default:
	}
	return b.r.Read(p)
}

func (b *pipeBody) Close() error {
	b.closed.Add(1)
	return b.r.Close()
}

// TestRetry_BufferingHonorsContext checks that buffering a replayable body
// that never ends stops when the request's context ends — before the read or
// while it is blocked — with the context's error, the body closed once and
// one failed request reported; an ErrorHandler sees the failure once, with
// zero attempts.
func TestRetry_BufferingHonorsContext(t *testing.T) {
	t.Parallel()

	for _, during := range []bool{false, true} {
		name := "ended before buffering"
		if during {
			name = "ended during buffering"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			obs := &recordingObserver{}
			var handled, tries atomic.Int32
			c := httpclient.New(
				httpclient.WithoutSSRFProtection(),
				httpclient.WithClient(&http.Client{Transport: unreachable(t)}),
				httpclient.WithObserver(obs),
				httpclient.WithRetryMax(3),
				httpclient.WithErrorHandler(func(resp *http.Response, err error, numTries int) (*http.Response, error) {
					handled.Add(1)
					tries.Store(int32(numTries)) //nolint:gosec // a small count
					if resp != nil {
						return nil, errors.New("the handler received a response")
					}
					return nil, err
				}),
			)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			body := newPipeBody(t)
			if during {
				go func() {
					<-body.reads
					cancel()
				}()
			} else {
				cancel()
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://api.test/x", body)
			require.NoError(t, err)

			resp, err := c.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			require.ErrorIs(t, err, context.Canceled)
			require.EqualValues(t, 1, body.closed.Load(), "the body is closed once")
			require.EqualValues(t, 1, handled.Load(), "the ErrorHandler sees the failure once")
			require.Zero(t, tries.Load(), "no attempt was sent")
			select {
			case <-body.reads:
				require.True(t, during, "no read starts on an ended context")
			default:
			}

			obs.mu.Lock()
			defer obs.mu.Unlock()
			require.Len(t, obs.done, 1, "one completion")
			require.True(t, obs.done[0].Failed)
		})
	}
}
