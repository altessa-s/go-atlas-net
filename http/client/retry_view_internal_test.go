// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/http/client/limiters"
	"github.com/altessa-s/go-atlas-net/httpx"

	coreretry "github.com/altessa-s/go-atlas-core/retry"
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

// TestRetryRoundTripper_AwaitsPreviousLiveBody checks that a retry reusing
// the caller's live trailers waits until the previous attempt's body, still
// being read when its reply came back, is closed.
func TestRetryRoundTripper_AwaitsPreviousLiveBody(t *testing.T) {
	t.Parallel()

	var (
		calls       int
		firstClosed atomic.Bool
		closedAtTwo bool
	)

	req := newTrailerRequest(t)

	rt := &retryRoundTripper{
		observer:          NoopObserver{},
		retryableStatuses: DefaultRetryableStatuses(),
		next: httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				// The reply returns while the upload still holds the body.
				body := r.Body

				go func() {
					time.Sleep(50 * time.Millisecond)

					buf := make([]byte, 2)
					_, _ = body.Read(buf) // a partial read: the body never reaches EOF

					firstClosed.Store(true)

					_ = body.Close()
				}()

				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Request: r}, nil
			}

			closedAtTwo = firstClosed.Load()

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
	require.True(t, closedAtTwo, "the retry started only after the first body was closed")
}

// TestRetryRoundTripper_BufferFailureReportsSnapshot checks that a buffering
// failure is reported for the snapshot, even when reading the body edited the
// caller's request.
func TestRetryRoundTripper_BufferFailureReportsSnapshot(t *testing.T) {
	t.Parallel()

	var hosts []string

	rt := &retryRoundTripper{
		observer:          hostRecorder{hosts: &hosts},
		retryableStatuses: DefaultRetryableStatuses(),
		next: httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) {
			t.Error("no attempt is made")
			return nil, errors.New("unreachable")
		}),
		retryOpts:   newTestRetryOpts(2),
		maxAttempts: 2,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "http://example.com/path", nil)
	require.NoError(t, err)

	req.Body = io.NopCloser(readerFunc(func([]byte) (int, error) {
		req.URL = &url.URL{Scheme: "http", Host: "edited.test"}
		return 0, errors.New("read failed")
	}))

	_, err = rt.RoundTrip(req) //nolint:bodyclose // a failed round trip returns no response
	require.Error(t, err)
	require.Equal(t, []string{"example.com"}, hosts)
}

// readerFunc adapts a function to io.Reader.
type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// TestRetryRoundTripper_AwaitsRewoundBody checks that every body that can
// write the live trailers is waited for: the transport rewinds the body
// itself, replies early and closes the replacement late; the caller's GetBody
// is asked for the retry's body only after that replacement is closed.
func TestRetryRoundTripper_AwaitsRewoundBody(t *testing.T) {
	t.Parallel()

	var (
		calls          int
		getBodies      atomic.Int32
		replacementOff atomic.Bool
		closedAtAsk    atomic.Bool
	)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "http://example.com", nil)
	require.NoError(t, err)

	req.Trailer = http.Header{"X-Sum": nil}
	req.Body = &trailerBody{r: strings.NewReader("payload"), trailer: req.Trailer}
	req.GetBody = func() (io.ReadCloser, error) {
		if getBodies.Add(1) == 2 { // the retry layer's own rewind
			closedAtAsk.Store(replacementOff.Load())
		}

		return &trailerBody{r: strings.NewReader("payload"), trailer: req.Trailer}, nil
	}

	rt := &retryRoundTripper{
		observer:          NoopObserver{},
		retryableStatuses: DefaultRetryableStatuses(),
		next: httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				_ = r.Body.Close()

				replacement, gbErr := r.GetBody() // as net/http rewinds on a reused connection
				require.NoError(t, gbErr)

				go func() {
					time.Sleep(50 * time.Millisecond)

					buf := make([]byte, 2)
					_, _ = replacement.Read(buf)

					replacementOff.Store(true)

					_ = replacement.Close()
				}()

				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Request: r}, nil
			}

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
	require.Equal(t, int32(2), getBodies.Load())
	require.True(t, closedAtAsk.Load(), "GetBody was asked only after the replacement was closed")
}

// deferredDoer answers at once and keeps every request body unread, as a
// transport does whose reply arrives before it has sent the upload.
type deferredDoer struct{ bodies []io.ReadCloser }

func (d *deferredDoer) Do(req *http.Request) (*http.Response, error) {
	d.bodies = append(d.bodies, req.Body)
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

// TestDoJSONRequest_RequestOwnsBody checks that a JSON request owns its
// encoded bytes: a body still unsent when the call returns is not changed by
// the next call reusing the pooled buffer.
func TestDoJSONRequest_RequestOwnsBody(t *testing.T) {
	t.Parallel()

	d := &deferredDoer{}

	for _, v := range []string{"aaaa", "bbbb"} {
		resp, err := doJSONRequest(t.Context(), d, http.MethodPost, "http://example.com", map[string]string{"v": v})
		require.NoError(t, err)

		_ = resp.Body.Close()
	}

	require.Len(t, d.bodies, 2)

	for i, want := range []string{`{"v":"aaaa"}` + "\n", `{"v":"bbbb"}` + "\n"} {
		got, err := io.ReadAll(d.bodies[i])
		require.NoError(t, err)
		require.Equal(t, want, string(got))
	}
}

// TestRetryRoundTripper_TracksBodiesAfterFinalTrailers checks that every
// caller body stays tracked once the trailers are final: the first attempt
// reads its body to the end, the second replies early and closes its body
// late, and the caller's GetBody is asked for the third attempt's body only
// after that.
func TestRetryRoundTripper_TracksBodiesAfterFinalTrailers(t *testing.T) {
	t.Parallel()

	var (
		calls        int
		getBodies    atomic.Int32
		secondClosed atomic.Bool
		closedAtAsk  atomic.Bool
	)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "http://example.com", nil)
	require.NoError(t, err)

	req.Trailer = http.Header{"X-Sum": nil}
	req.Body = &trailerBody{r: strings.NewReader("payload"), trailer: req.Trailer}
	req.GetBody = func() (io.ReadCloser, error) {
		if getBodies.Add(1) == 2 { // the third attempt's body
			closedAtAsk.Store(secondClosed.Load())
		}

		return &trailerBody{r: strings.NewReader("payload"), trailer: req.Trailer}, nil
	}

	rt := &retryRoundTripper{
		observer:          NoopObserver{},
		retryableStatuses: DefaultRetryableStatuses(),
		next: httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			calls++

			switch calls {
			case 1:
				_, _ = io.Copy(io.Discard, r.Body) // the trailers become final
				_ = r.Body.Close()
			case 2:
				body := r.Body

				go func() {
					time.Sleep(50 * time.Millisecond)

					buf := make([]byte, 2)
					_, _ = body.Read(buf)

					secondClosed.Store(true)

					_ = body.Close()
				}()
			default:
				_, _ = io.Copy(io.Discard, r.Body)
				_ = r.Body.Close()

				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
			}

			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Request: r}, nil
		}),
		retryOpts:   newTestRetryOpts(2),
		maxAttempts: 2,
	}

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)

	_ = resp.Body.Close()

	require.Equal(t, 3, calls)
	require.True(t, closedAtAsk.Load(), "GetBody was asked only after the second body was closed")
}

// TestRetryRoundTripper_AttemptOwnsTrailers checks that the transport never
// sees the caller's trailer map: with an early reply, the transport reads the
// body to its end, closes it and only then writes the trailers, from the
// attempt's own map, which the hook filled at the body's end.
func TestRetryRoundTripper_AttemptOwnsTrailers(t *testing.T) {
	t.Parallel()

	var (
		done       = make(chan struct{})
		sameAsLive bool
		written    string
	)

	req := newTrailerRequest(t)
	req.Method = http.MethodPost // not eligible: one attempt

	rt := &retryRoundTripper{
		observer:          NoopObserver{},
		retryableStatuses: DefaultRetryableStatuses(),
		next: httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			go func() {
				defer close(done)

				_, _ = io.Copy(io.Discard, r.Body)
				_ = r.Body.Close()

				sameAsLive = reflect.ValueOf(r.Trailer).Pointer() == reflect.ValueOf(req.Trailer).Pointer()
				written = r.Trailer.Get("X-Sum")
			}()

			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
		}),
		retryOpts:   newTestRetryOpts(2),
		maxAttempts: 2,
	}

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)

	_ = resp.Body.Close()

	<-done

	require.False(t, sameAsLive, "the attempt has its own trailer map")
	require.Equal(t, "abc", written, "the hook copied the final values before EOF")
	require.Equal(t, "abc", req.Trailer.Get("X-Sum"), "the caller's body filled the caller's map")
}

// TestRetryRoundTripper_ViewIsDeepCopy checks that editing the view's form,
// path values and URL user info reaches no attempt request.
func TestRetryRoundTripper_ViewIsDeepCopy(t *testing.T) {
	t.Parallel()

	var sent *http.Request

	rt := &retryRoundTripper{
		observer:          NoopObserver{},
		retryableStatuses: DefaultRetryableStatuses(),
		retryPolicyHandler: func(_ context.Context, resp *http.Response, _ error) (bool, error) {
			resp.Request.Form.Set("f", "evil")
			resp.Request.SetPathValue("p", "evil")
			*resp.Request.URL.User = *url.UserPassword("evil", "evil")

			return false, nil
		},
		next: httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			sent = r
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
		}),
		retryOpts:   newTestRetryOpts(2),
		maxAttempts: 2,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://user:pw@example.com/", nil)
	require.NoError(t, err)

	req.Form = url.Values{"f": {"v"}}
	req.SetPathValue("p", "v")

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)

	_ = resp.Body.Close()

	require.Equal(t, "v", sent.Form.Get("f"))
	require.Equal(t, "v", sent.PathValue("p"))
	require.Equal(t, "user:pw", sent.URL.User.String())
	require.Equal(t, "user:pw", req.URL.User.String())
}

// funcBody is a reply body of a type that cannot be compared (it holds a
// func), counting its Close calls.
type funcBody struct {
	io.Reader

	onClose func()
}

func (b funcBody) Close() error {
	b.onClose()
	return nil
}

// TestRetryRoundTripper_ReplyBodyOwnership checks the disposal of discarded
// replies: every received body is closed exactly once, whatever a policy did
// to resp.Body, a body of a non-comparable type never panics, and a
// cancellation during the backoff does not close it twice.
func TestRetryRoundTripper_ReplyBodyOwnership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		policy RetryPolicyHandler
		cancel bool
	}{
		{name: "success on the next attempt"},
		{name: "policy clears the body", policy: func(_ context.Context, resp *http.Response, _ error) (bool, error) {
			if resp.StatusCode == http.StatusServiceUnavailable {
				resp.Body = nil
				return true, nil
			}

			return false, nil
		}},
		{name: "policy replaces the body", policy: func(_ context.Context, resp *http.Response, _ error) (bool, error) {
			if resp.StatusCode == http.StatusServiceUnavailable {
				resp.Body = io.NopCloser(strings.NewReader("replacement"))
				return true, nil
			}

			return false, nil
		}},
		{name: "canceled during the backoff", cancel: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var (
				calls  int
				closes atomic.Int32
			)

			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)

			opts := newTestRetryOpts(2)
			if tc.cancel {
				opts = []coreretry.Option{
					coreretry.WithMaxAttempts(2),
					coreretry.WithNextDelay(func(int, error) time.Duration {
						cancel()
						return time.Hour
					}),
				}
			}

			rt := &retryRoundTripper{
				observer:           NoopObserver{},
				retryableStatuses:  DefaultRetryableStatuses(),
				retryPolicyHandler: tc.policy,
				next: httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						body := funcBody{Reader: strings.NewReader("busy"), onClose: func() { closes.Add(1) }}
						return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: body, Request: r}, nil
					}

					return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
				}),
				retryOpts:   opts,
				maxAttempts: 2,
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com", nil)
			require.NoError(t, err)

			resp, err := rt.RoundTrip(req)
			if resp != nil {
				_ = resp.Body.Close()
			}

			if tc.cancel {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			require.Equal(t, int32(1), closes.Load(), "the received body is closed exactly once")
		})
	}
}

// TestRetryRoundTripper_ExhaustedFromReceivedReply checks that the exhausted
// error describes the reply as received: a policy editing the reply's status
// and clearing its request view changes nothing in it.
func TestRetryRoundTripper_ExhaustedFromReceivedReply(t *testing.T) {
	t.Parallel()

	rt := &retryRoundTripper{
		observer:          NoopObserver{},
		retryableStatuses: DefaultRetryableStatuses(),
		retryPolicyHandler: func(_ context.Context, resp *http.Response, _ error) (bool, error) {
			resp.StatusCode = http.StatusTeapot
			resp.Request = nil

			return true, nil
		},
		next: httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Request: r}, nil
		}),
		retryOpts:   newTestRetryOpts(1),
		maxAttempts: 1,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/path?q=secret", nil)
	require.NoError(t, err)

	_, err = rt.RoundTrip(req) //nolint:bodyclose // an exhausted round trip returns no response
	exhaustedErr, ok := errors.AsType[*RetryExhaustedError](err)
	require.True(t, ok, "error %v", err)

	status, ok := errors.AsType[*UnexpectedStatusError](exhaustedErr.LastError)
	require.True(t, ok, "last error %v", exhaustedErr.LastError)
	require.Equal(t, &UnexpectedStatusError{Status: http.StatusServiceUnavailable, Method: http.MethodGet, Host: "example.com", URI: "/path"}, status)
}
