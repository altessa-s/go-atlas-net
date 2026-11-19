// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"sync"
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

// gatedBody is a caller body whose first Read blocks until release is closed,
// and whose Close does not unblock it.
type gatedBody struct {
	release chan struct{}
	r       io.Reader
	trailer http.Header
	done    *atomic.Bool  // set as the Read returns
	started chan struct{} // closed once a Read has begun (and is registered)
	once    sync.Once
}

func (b *gatedBody) Read(p []byte) (int, error) {
	if b.started != nil {
		b.once.Do(func() { close(b.started) })
	}

	<-b.release

	n, err := b.r.Read(p)
	if errors.Is(err, io.EOF) {
		b.trailer.Set("X-Sum", "abc")
	}

	b.done.Store(true)

	return n, err
}

func (b *gatedBody) Close() error { return nil }

// TestRetryRoundTripper_TransportGetBodyAwaitsReads checks that a transport's
// own rewind waits, like the retry layer's, until an earlier body that may
// write the caller's trailers has no Read in flight, even once it is closed.
func TestRetryRoundTripper_TransportGetBodyAwaitsReads(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})

	var (
		readDone   atomic.Bool
		doneAtCall atomic.Bool
	)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "http://example.com", nil)
	require.NoError(t, err)

	req.Trailer = http.Header{"X-Sum": nil}
	started := make(chan struct{})
	req.Body = &gatedBody{release: release, r: strings.NewReader("payload"), trailer: req.Trailer, done: &readDone, started: started}
	req.GetBody = func() (io.ReadCloser, error) {
		doneAtCall.Store(readDone.Load())
		return &trailerBody{r: strings.NewReader("payload"), trailer: req.Trailer}, nil
	}

	rt := &retryRoundTripper{
		observer:          NoopObserver{},
		retryableStatuses: DefaultRetryableStatuses(),
		next: httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			body := r.Body

			go func() {
				buf := make([]byte, 64)
				_, _ = body.Read(buf) // held until release
			}()

			<-started // the Read is in flight, registered by the hook

			_ = body.Close() // closed while the Read is still in flight

			go func() {
				time.Sleep(50 * time.Millisecond)
				close(release)
			}()

			replacement, gbErr := r.GetBody() // net/http's own rewind
			require.NoError(t, gbErr)

			_, _ = io.Copy(io.Discard, replacement)
			_ = replacement.Close()

			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
		}),
		retryOpts:   newTestRetryOpts(0),
		maxAttempts: 0,
	}

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)

	_ = resp.Body.Close()

	require.True(t, doneAtCall.Load(), "the caller's GetBody ran only after the earlier Read returned")
}

// countingBody is a transport reply body counting its Close calls.
type countingBody struct {
	io.Reader

	closes *atomic.Int32
}

func (b countingBody) Close() error {
	b.closes.Add(1)
	return nil
}

// TestRetryRoundTripper_ReturnedBodyOwnership checks the one ownership rule
// for the reply handed back: the received body passes to the caller when it
// is the body returned, and is otherwise released when the returned body is
// closed (never before, never read), whoever made the returned reply.
func TestRetryRoundTripper_ReturnedBodyOwnership(t *testing.T) {
	t.Parallel()

	replaceWithPipe := func(resp *http.Response) *io.PipeWriter {
		pr, pw := io.Pipe()
		resp.Body = pr

		return pw
	}

	type outcome struct {
		policy  RetryPolicyHandler
		handler ErrorHandler
		status  int
		want    string
	}

	// keep feeds a replacement pipe.
	keep := func(pw *io.PipeWriter) {
		go func() {
			_, _ = io.WriteString(pw, "replacement")
			_ = pw.Close()
		}()
	}

	tests := map[string]outcome{
		"reply returned as is": {status: http.StatusOK, want: "received"},
		"policy replaces the body": {status: http.StatusOK, want: "replacement",
			policy: func(_ context.Context, resp *http.Response, _ error) (bool, error) {
				keep(replaceWithPipe(resp))
				return false, nil
			}},
		"handler replaces the body of the same reply": {status: http.StatusServiceUnavailable, want: "replacement",
			handler: func(resp *http.Response, _ error, _ int) (*http.Response, error) {
				keep(replaceWithPipe(resp))
				return resp, nil
			}},
		"handler returns its own reply": {status: http.StatusServiceUnavailable, want: "replacement",
			handler: func(_ *http.Response, _ error, _ int) (*http.Response, error) {
				own := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
				keep(replaceWithPipe(own))

				return own, nil
			}},
		"handler wraps the received body": {status: http.StatusServiceUnavailable, want: "received",
			handler: func(resp *http.Response, _ error, _ int) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(resp.Body)}, nil
			}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var closes atomic.Int32

			rt := &retryRoundTripper{
				observer:           NoopObserver{},
				retryableStatuses:  DefaultRetryableStatuses(),
				retryPolicyHandler: tc.policy,
				errorHandler:       tc.handler,
				next: httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: tc.status,
						Body:       countingBody{Reader: strings.NewReader("received"), closes: &closes},
						Request:    r,
					}, nil
				}),
				retryOpts:   newTestRetryOpts(0),
				maxAttempts: 0,
			}

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com", nil)
			require.NoError(t, err)

			resp, err := rt.RoundTrip(req)
			require.NoError(t, err)
			require.Zero(t, closes.Load(), "nothing is released before the caller closes the reply")

			got, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, tc.want, string(got))

			require.NoError(t, resp.Body.Close())
			require.Equal(t, int32(1), closes.Load(), "the received body is released exactly once")
		})
	}
}

// TestRetryRoundTripper_TransportRewindBoundedByAttempt checks that a
// transport rewind after it closed the attempt's body, whose Read never
// finishes, gives up at the attempt's deadline, though the request's context
// lives on.
func TestRetryRoundTripper_TransportRewindBoundedByAttempt(t *testing.T) {
	t.Parallel()

	never := make(chan struct{})

	t.Cleanup(func() { close(never) })

	var readDone atomic.Bool

	started := make(chan struct{})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "http://example.com", nil)
	require.NoError(t, err)

	req.Trailer = http.Header{"X-Sum": nil}
	req.Body = &gatedBody{release: never, r: strings.NewReader("payload"), trailer: req.Trailer, done: &readDone, started: started}
	req.GetBody = func() (io.ReadCloser, error) {
		return &trailerBody{r: strings.NewReader("payload"), trailer: req.Trailer}, nil
	}

	var (
		rewindErr error
		waited    time.Duration
	)

	rt := &retryRoundTripper{
		observer:          NoopObserver{},
		retryableStatuses: DefaultRetryableStatuses(),
		attemptTimeout:    100 * time.Millisecond,
		next: httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			body := r.Body

			go func() {
				buf := make([]byte, 64)
				_, _ = body.Read(buf) // never returns
			}()

			<-started

			_ = body.Close() // the rewind follows a Close, with a Read still in flight

			begin := time.Now()
			_, rewindErr = r.GetBody()
			waited = time.Since(begin)

			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
		}),
		retryOpts:   newTestRetryOpts(0),
		maxAttempts: 0,
	}

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)

	_ = resp.Body.Close()

	require.ErrorIs(t, rewindErr, context.DeadlineExceeded)
	require.Less(t, waited, 5*time.Second)
	require.NoError(t, t.Context().Err(), "the request's context is still live")
}

// TestRetryRoundTripper_HandedBodyReleased checks that the body a policy put
// in place of the reply's, handed to the ErrorHandler, is released once when
// the handler returns only an error, and the received body with it.
func TestRetryRoundTripper_HandedBodyReleased(t *testing.T) {
	t.Parallel()

	var transportCloses, replacementCloses atomic.Int32

	rt := &retryRoundTripper{
		observer:          NoopObserver{},
		retryableStatuses: DefaultRetryableStatuses(),
		retryPolicyHandler: func(_ context.Context, resp *http.Response, _ error) (bool, error) {
			pr, pw := io.Pipe()
			_ = pw.Close()
			resp.Body = countingBody{Reader: pr, closes: &replacementCloses}

			return true, nil
		},
		errorHandler: func(*http.Response, error, int) (*http.Response, error) {
			return nil, errors.New("handled")
		},
		next: httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       countingBody{Reader: strings.NewReader("busy"), closes: &transportCloses},
				Request:    r,
			}, nil
		}),
		retryOpts:   newTestRetryOpts(0),
		maxAttempts: 0,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com", nil)
	require.NoError(t, err)

	_, err = rt.RoundTrip(req) //nolint:bodyclose // a failed round trip returns no response
	require.Error(t, err)
	require.Equal(t, int32(1), replacementCloses.Load(), "the handed replacement is released once")
	require.Equal(t, int32(1), transportCloses.Load(), "the received body is released once")
}

// TestDriverUpgrade_CloseWrite checks that a 101 body through a transport
// driver keeps its CloseWrite (the pool forwards it): the backend answers
// only after the client half-closed the write side.
func TestDriverUpgrade_CloseWrite(t *testing.T) {
	t.Parallel()

	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()

		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		_ = brw.Flush()

		got, _ := io.ReadAll(brw) // until the client half-closes
		_, _ = conn.Write(append([]byte("got "), got...))
	}))
	t.Cleanup(s.Close)

	c := New(WithTransport(driverBase(s)), WithTransportDriver(driverFunc(standardDriver)), WithoutRetry(),
		WithSSRFAllowedCIDRs(netip.MustParsePrefix("127.0.0.0/8")))
	t.Cleanup(c.CloseIdleConnections)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "echo")

	resp, err := c.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	rw, ok := resp.Body.(io.ReadWriteCloser)
	require.True(t, ok, "writable, got %T", resp.Body)

	cw, ok := resp.Body.(interface{ CloseWrite() error })
	require.True(t, ok, "CloseWrite kept, got %T", resp.Body)

	_, err = rw.Write([]byte("ping"))
	require.NoError(t, err)
	require.NoError(t, cw.CloseWrite())

	reply, err := io.ReadAll(rw)
	require.NoError(t, err)
	require.Equal(t, "got ping", string(reply))
}

// TestRetryRoundTripper_PolicyReplacementsNeverCompared checks replacement
// bodies a policy supplies that cannot be compared (a func-backed reader in
// io.NopCloser, a value type holding a func): no panic, the replacement
// closed exactly once, the received body released exactly once.
func TestRetryRoundTripper_PolicyReplacementsNeverCompared(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		status int // of the first reply; 503 is retried, 200 returned
	}{
		"returned to the caller": {status: http.StatusOK},
		"discarded for a retry":  {status: http.StatusServiceUnavailable},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var (
				calls             int
				transportCloses   atomic.Int32
				replacementCloses atomic.Int32
			)

			rt := &retryRoundTripper{
				observer:          NoopObserver{},
				retryableStatuses: DefaultRetryableStatuses(),
				retryPolicyHandler: func(_ context.Context, resp *http.Response, _ error) (bool, error) {
					if calls > 1 {
						return false, nil
					}

					if resp.StatusCode == http.StatusOK {
						resp.Body = funcBody{
							Reader:  io.NopCloser(readerFunc(func([]byte) (int, error) { return 0, io.EOF })),
							onClose: func() { replacementCloses.Add(1) },
						}

						return false, nil
					}

					resp.Body = funcBody{Reader: strings.NewReader("x"), onClose: func() { replacementCloses.Add(1) }}

					return true, nil
				},
				next: httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
					calls++

					status := tc.status
					if calls > 1 {
						status = http.StatusOK
					}

					return &http.Response{
						StatusCode: status,
						Body:       countingBody{Reader: strings.NewReader("received"), closes: &transportCloses},
						Request:    r,
					}, nil
				}),
				retryOpts:   newTestRetryOpts(2),
				maxAttempts: 2,
			}

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com", nil)
			require.NoError(t, err)

			resp, err := rt.RoundTrip(req)
			require.NoError(t, err)

			_, _ = io.Copy(io.Discard, resp.Body)
			require.NoError(t, resp.Body.Close())

			require.Equal(t, int32(1), replacementCloses.Load(), "the replacement is closed exactly once")

			wantTransport := int32(1)
			if tc.status == http.StatusServiceUnavailable {
				wantTransport = 2 // the discarded reply and the returned one
			}

			require.Equal(t, wantTransport, transportCloses.Load())
		})
	}
}

// TestRetryRoundTripper_GetBodyWhileOriginalOpen checks that a transport may
// read a GetBody copy while the attempt's body is still open, then send the
// original: no wait, with static trailers.
func TestRetryRoundTripper_GetBodyWhileOriginalOpen(t *testing.T) {
	t.Parallel()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "http://example.com", strings.NewReader("payload"))
	require.NoError(t, err)

	req.Trailer = http.Header{"X-Sum": {"static"}}

	var copied, sent string

	rt := &retryRoundTripper{
		observer:          NoopObserver{},
		retryableStatuses: DefaultRetryableStatuses(),
		next: httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			cp, gbErr := r.GetBody()
			if gbErr != nil {
				return nil, gbErr
			}

			b, _ := io.ReadAll(cp)
			_ = cp.Close()
			copied = string(b)

			b, _ = io.ReadAll(r.Body)
			_ = r.Body.Close()
			sent = string(b)

			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
		}),
		retryOpts:   newTestRetryOpts(0),
		maxAttempts: 0,
	}

	done := make(chan error, 1)

	go func() {
		resp, rtErr := rt.RoundTrip(req)
		if resp != nil {
			_ = resp.Body.Close()
		}

		done <- rtErr
	}()

	select {
	case rtErr := <-done:
		require.NoError(t, rtErr)
	case <-time.After(5 * time.Second):
		t.Fatal("GetBody waited for the open original body")
	}

	require.Equal(t, "payload", copied)
	require.Equal(t, "payload", sent)
}

// TestRetryRoundTripper_HandlerLeftoverBody checks the body an ErrorHandler
// puts on the reply it was given: closed once when the handler returns only
// an error, released once when it returns a reply of its own.
func TestRetryRoundTripper_HandlerLeftoverBody(t *testing.T) {
	t.Parallel()

	for name, fail := range map[string]bool{"handler fails": true, "handler returns its own reply": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var transportCloses, leftoverCloses atomic.Int32

			rt := &retryRoundTripper{
				observer:          NoopObserver{},
				retryableStatuses: DefaultRetryableStatuses(),
				errorHandler: func(resp *http.Response, _ error, _ int) (*http.Response, error) {
					resp.Body = countingBody{Reader: strings.NewReader("leftover"), closes: &leftoverCloses}

					if fail {
						return nil, errors.New("handled")
					}

					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("own"))}, nil
				},
				next: httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusServiceUnavailable,
						Body:       countingBody{Reader: strings.NewReader("busy"), closes: &transportCloses},
						Request:    r,
					}, nil
				}),
				retryOpts:   newTestRetryOpts(0),
				maxAttempts: 0,
			}

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com", nil)
			require.NoError(t, err)

			resp, err := rt.RoundTrip(req)
			if fail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)

				got, _ := io.ReadAll(resp.Body)
				require.Equal(t, "own", string(got))
				require.NoError(t, resp.Body.Close())
			}

			require.Equal(t, int32(1), leftoverCloses.Load(), "the handler's leftover body is released once")
			require.Equal(t, int32(1), transportCloses.Load(), "the received body is released once")
		})
	}
}
