// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sony/gobreaker/v2"
	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-core/time/clock"

	"github.com/altessa-s/go-atlas-net/httpx"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

// echoedRequest is what echoServer reports about the request it received.
type echoedRequest struct {
	Method      string              `json:"method"`
	Path        string              `json:"path"`
	Query       map[string][]string `json:"query"`
	ContentType string              `json:"contentType"`
	Body        string              `json:"body"`
	Header      map[string][]string `json:"header"`
}

// echoServer answers every request with a JSON description of it and counts
// the requests it served.
func echoServer(tb testing.TB) (*httptest.Server, *atomic.Int32) {
	tb.Helper()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(echoedRequest{
			Method:      r.Method,
			Path:        r.URL.Path,
			Query:       r.URL.Query(),
			ContentType: r.Header.Get("Content-Type"),
			Body:        string(body),
			Header:      r.Header,
		})
	}))
	tb.Cleanup(srv.Close)
	return srv, &hits
}

func decodeEcho(tb testing.TB, resp *http.Response, err error) echoedRequest {
	tb.Helper()

	require.NoError(tb, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(tb, http.StatusOK, resp.StatusCode)
	var got echoedRequest
	require.NoError(tb, json.NewDecoder(resp.Body).Decode(&got))
	return got
}

func TestHTTPClient_VerbHelpers(t *testing.T) {
	t.Parallel()

	srv, _ := echoServer(t)
	c := httpclient.NewHTTPClient(loopback, httpclient.WithRetryMax(0))
	payload := map[string]string{"name": "ada"}
	const jsonBody = "{\"name\":\"ada\"}\n"

	cases := []struct {
		name            string
		do              func(ctx context.Context) (*http.Response, error)
		wantMethod      string
		wantContentType string
		wantBody        string
	}{
		{
			name: "post",
			do: func(ctx context.Context) (*http.Response, error) {
				return c.Post(ctx, srv.URL+"/r", "text/plain", strings.NewReader("hi"))
			},
			wantMethod:      http.MethodPost,
			wantContentType: "text/plain",
			wantBody:        "hi",
		},
		{
			name: "put",
			do: func(ctx context.Context) (*http.Response, error) {
				return c.Put(ctx, srv.URL+"/r", "text/csv", strings.NewReader("a,b"))
			},
			wantMethod:      http.MethodPut,
			wantContentType: "text/csv",
			wantBody:        "a,b",
		},
		{
			name: "patch",
			do: func(ctx context.Context) (*http.Response, error) {
				return c.Patch(ctx, srv.URL+"/r", "text/plain", strings.NewReader("p"))
			},
			wantMethod:      http.MethodPatch,
			wantContentType: "text/plain",
			wantBody:        "p",
		},
		{
			name:            "put_json",
			do:              func(ctx context.Context) (*http.Response, error) { return c.PutJSON(ctx, srv.URL+"/r", payload) },
			wantMethod:      http.MethodPut,
			wantContentType: "application/json",
			wantBody:        jsonBody,
		},
		{
			name:            "patch_json",
			do:              func(ctx context.Context) (*http.Response, error) { return c.PatchJSON(ctx, srv.URL+"/r", payload) },
			wantMethod:      http.MethodPatch,
			wantContentType: "application/json",
			wantBody:        jsonBody,
		},
		{
			name: "post_form",
			do: func(ctx context.Context) (*http.Response, error) {
				return c.PostForm(ctx, srv.URL+"/r", url.Values{"user": {"ada"}, "tag": {"a b"}})
			},
			wantMethod:      http.MethodPost,
			wantContentType: "application/x-www-form-urlencoded",
			wantBody:        "tag=a+b&user=ada",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resp, err := tc.do(t.Context())
			got := decodeEcho(t, resp, err)
			require.Equal(t, tc.wantMethod, got.Method)
			require.Equal(t, "/r", got.Path)
			require.Equal(t, tc.wantContentType, got.ContentType)
			require.Equal(t, tc.wantBody, got.Body)
		})
	}
}

func TestHTTPClient_WithOptionsHelpers(t *testing.T) {
	t.Parallel()

	srv, _ := echoServer(t)
	c := httpclient.NewHTTPClient(loopback, httpclient.WithRetryMax(0))
	opts := []httpclient.RequestOption{
		httpclient.WithHeaders(maps.All(map[string]string{"X-Tenant": "acme", "X-Trace": "t-1"})),
		httpclient.WithQueryParams(url.Values{"page": {"2"}, "tag": {"x", "y"}}),
	}

	cases := []struct {
		name       string
		do         func(ctx context.Context) (*http.Response, error)
		wantMethod string
		wantBody   string
	}{
		{
			name: "post",
			do: func(ctx context.Context) (*http.Response, error) {
				return c.PostWithOptions(ctx, srv.URL+"/r?keep=1", "text/plain", strings.NewReader("b"), opts...)
			},
			wantMethod: http.MethodPost,
			wantBody:   "b",
		},
		{
			name: "put",
			do: func(ctx context.Context) (*http.Response, error) {
				return c.PutWithOptions(ctx, srv.URL+"/r?keep=1", "text/plain", strings.NewReader("b"), opts...)
			},
			wantMethod: http.MethodPut,
			wantBody:   "b",
		},
		{
			name: "delete",
			do: func(ctx context.Context) (*http.Response, error) {
				return c.DeleteWithOptions(ctx, srv.URL+"/r?keep=1", opts...)
			},
			wantMethod: http.MethodDelete,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resp, err := tc.do(t.Context())
			got := decodeEcho(t, resp, err)
			require.Equal(t, tc.wantMethod, got.Method)
			require.Equal(t, tc.wantBody, got.Body)
			require.Equal(t, []string{"acme"}, got.Header["X-Tenant"])
			require.Equal(t, []string{"t-1"}, got.Header["X-Trace"])
			require.Equal(t, map[string][]string{"keep": {"1"}, "page": {"2"}, "tag": {"x", "y"}}, got.Query,
				"options merge into the query string without dropping existing parameters")
		})
	}
}

func TestHTTPClient_JSONHelpersRejectUnencodableBody(t *testing.T) {
	t.Parallel()

	srv, hits := echoServer(t)
	c := httpclient.NewHTTPClient(loopback, httpclient.WithRetryMax(0))
	unencodable := map[string]any{"ch": make(chan int)}

	for name, do := range map[string]func(ctx context.Context) (*http.Response, error){
		"put":   func(ctx context.Context) (*http.Response, error) { return c.PutJSON(ctx, srv.URL, unencodable) },
		"patch": func(ctx context.Context) (*http.Response, error) { return c.PatchJSON(ctx, srv.URL, unencodable) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			resp, err := do(t.Context())
			if resp != nil {
				_ = resp.Body.Close()
			}
			var jerr *json.UnsupportedTypeError
			require.ErrorAs(t, err, &jerr)
		})
	}
	t.Cleanup(func() { require.Zero(t, hits.Load(), "a body that cannot be encoded must not be sent") })
}

func TestRequestBuilder_FormHeadersAndQuery(t *testing.T) {
	t.Parallel()

	srv, _ := echoServer(t)
	c := httpclient.NewHTTPClient(loopback, httpclient.WithRetryMax(0))

	builders := map[string]func() httpclient.RequestBuilder{
		"client_interned": c.NewRequest,
		"standalone":      func() httpclient.RequestBuilder { return httpclient.NewRequestBuilder(c) },
	}
	for name, newBuilder := range builders {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			resp, err := newBuilder().
				POST(srv.URL+"/form").
				FormBody(url.Values{"q": {"go & net"}}).
				Headers(maps.All(map[string]string{"X-A": "1", "X-B": "2"})).
				QueryParams(url.Values{"page": {"1"}}).
				QueryParamsSeq(maps.All(map[string]string{"sort": "asc"})).
				QueryParam("page", "2").
				Send(t.Context())
			got := decodeEcho(t, resp, err)

			require.Equal(t, http.MethodPost, got.Method)
			require.Equal(t, "application/x-www-form-urlencoded", got.ContentType)
			require.Equal(t, "q=go+%26+net", got.Body)
			require.Equal(t, []string{"1"}, got.Header["X-A"])
			require.Equal(t, []string{"2"}, got.Header["X-B"])
			require.Equal(t, map[string][]string{"page": {"1", "2"}, "sort": {"asc"}}, got.Query)
		})
	}
}

// trackingBody is an endless response body recording how much was read and
// whether it was closed.
type trackingBody struct {
	read   atomic.Int64
	closed atomic.Bool
}

func (b *trackingBody) Read(p []byte) (int, error) {
	b.read.Add(int64(len(p)))
	return len(p), nil
}

func (b *trackingBody) Close() error {
	b.closed.Store(true)
	return nil
}

func TestCloseBody(t *testing.T) {
	t.Parallel()

	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		require.NotPanics(t, func() { httpclient.CloseBody(nil) })
	})

	t.Run("drains_short_body", func(t *testing.T) {
		t.Parallel()
		r := strings.NewReader("leftover")
		var closed atomic.Bool
		httpclient.CloseBody(readCloser{Reader: r, close: func() error { closed.Store(true); return nil }})
		require.Zero(t, r.Len(), "a short body is drained so the connection can be reused")
		require.True(t, closed.Load())
	})

	t.Run("bounds_endless_body", func(t *testing.T) {
		t.Parallel()
		body := &trackingBody{}
		httpclient.CloseBody(body)
		require.True(t, body.closed.Load())
		require.Positive(t, body.read.Load())
		require.Less(t, body.read.Load(), int64(64<<20), "an endless body must not be drained forever")
	})
}

type readCloser struct {
	io.Reader
	close func() error
}

func (r readCloser) Close() error { return r.close() }

func TestUnexpectedStatusError_BodyPreview(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", httpclient.DefaultMaxBodySizeInError+10)
	cases := []struct {
		name         string
		body         []byte
		wantSuffix   string
		wantNoDetail bool
	}{
		{name: "no_body", wantNoDetail: true},
		{name: "short_body", body: []byte("quota exceeded"), wantSuffix: ". Details: quota exceeded"},
		{
			name:       "long_body_truncated",
			body:       []byte(long),
			wantSuffix: ". Details: " + long[:httpclient.DefaultMaxBodySizeInError] + "... (truncated)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := httpclient.UnexpectedStatusError{Status: http.StatusTooManyRequests, Host: "api.example", URI: "/v1", Method: http.MethodGet, Body: tc.body}
			msg := err.Error()
			require.True(t, strings.HasPrefix(msg, "unexpected response status: 429 (Too Many Requests); request: GET api.example/v1"), msg)
			if tc.wantNoDetail {
				require.NotContains(t, msg, "Details")
				return
			}
			require.True(t, strings.HasSuffix(msg, tc.wantSuffix))
		})
	}
}

// sleepRecorder is a fake clock that records every retry wait.
type sleepRecorder struct {
	*clock.Fake

	mu     sync.Mutex
	sleeps []time.Duration
}

func (c *sleepRecorder) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.mu.Unlock()
	return c.Fake.Sleep(ctx, d)
}

func TestRetryBackoffOptions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opts []httpclient.Option
		want []time.Duration
	}{
		{
			name: "min_factor_max",
			opts: []httpclient.Option{
				httpclient.WithRetryWaitMin(100 * time.Millisecond),
				httpclient.WithRetryWaitMax(250 * time.Millisecond),
				httpclient.WithRetryFactor(2),
			},
			want: []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 250 * time.Millisecond},
		},
		{
			name: "factor_three",
			opts: []httpclient.Option{
				httpclient.WithRetryWaitMin(10 * time.Millisecond),
				httpclient.WithRetryWaitMax(time.Second),
				httpclient.WithRetryFactor(3),
			},
			want: []time.Duration{10 * time.Millisecond, 30 * time.Millisecond, 90 * time.Millisecond},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.WriteHeader(http.StatusBadGateway)
			}))
			t.Cleanup(srv.Close)

			clk := &sleepRecorder{Fake: clock.NewFake(time.Now())}
			opts := append([]httpclient.Option{
				loopback, httpclient.WithRetryMax(3), httpclient.WithRetryJitter(0), httpclient.WithClock(clk),
			}, tc.opts...)
			c := httpclient.NewHTTPClient(opts...)

			resp, err := c.Get(t.Context(), srv.URL)
			if resp != nil {
				_ = resp.Body.Close()
			}
			require.ErrorIs(t, err, httpclient.ErrMaxRetriesExceeded)
			require.Equal(t, int32(4), hits.Load())
			clk.mu.Lock()
			defer clk.mu.Unlock()
			require.Equal(t, tc.want, clk.sleeps)
		})
	}
}

// breakerProbe is an observer that keeps the breakers view and counts
// rejections; the other events fall through to the embedded NoopObserver.
type breakerProbe struct {
	httpclient.NoopObserver

	breakers httpclient.Breakers
	rejected atomic.Int32
}

func (o *breakerProbe) Attach(b httpclient.Breakers) { o.breakers = b }

func (o *breakerProbe) BreakerRejected(httpclient.AttemptEvent) { o.rejected.Add(1) }

// TestBreakerMaxRequests pins how many successful half-open probes close a
// tripped breaker.
func TestBreakerMaxRequests(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		maxRequests uint32
		successes   int
		want        gobreaker.State
	}{
		{"one_probe_closes", 1, 1, gobreaker.StateClosed},
		{"default_needs_more_probes", httpclient.DefaultBreakerMaxRequests, 1, gobreaker.StateHalfOpen},
		{"default_closes_after_all_probes", httpclient.DefaultBreakerMaxRequests, httpclient.DefaultBreakerMaxRequests, gobreaker.StateClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				var healthy atomic.Bool
				rt := httpx.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
					if !healthy.Load() {
						return nil, errors.New("connection reset")
					}
					return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
				})
				probe := &breakerProbe{}
				c := httpclient.New(
					httpclient.WithoutSSRFProtection(),
					httpclient.WithClient(&http.Client{Transport: rt}),
					httpclient.WithoutRetry(),
					httpclient.WithBreakerTimeout(time.Minute),
					httpclient.WithBreakerMaxRequests(tc.maxRequests),
					httpclient.WithObserver(probe),
				)
				do := func() error {
					req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://svc.test/", nil)
					require.NoError(t, err)
					resp, err := c.Do(req)
					if resp != nil {
						_ = resp.Body.Close()
					}
					return err
				}

				for range httpclient.DefaultBreakerMinRequests {
					require.Error(t, do())
				}
				err := do()
				require.True(t, httpclient.IsCircuitBreakerOpen(err), "the tripped breaker must reject: %v", err)
				require.Equal(t, int32(1), probe.rejected.Load())
				require.Equal(t, gobreaker.StateOpen, probe.breakers.HostState("svc.test"))

				time.Sleep(time.Minute + time.Second)
				healthy.Store(true)
				for range tc.successes {
					require.NoError(t, do())
				}
				require.Equal(t, tc.want, probe.breakers.HostState("svc.test"))
			})
		})
	}
}

// syncWriter serializes writes from the breaker callback and the test.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestDefaultOnStateChange(t *testing.T) {
	t.Parallel()

	t.Run("logs_transitions_at_debug", func(t *testing.T) {
		t.Parallel()

		var out syncWriter
		logger := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
		rt := httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("connection reset") })
		c := httpclient.New(
			httpclient.WithoutSSRFProtection(),
			httpclient.WithClient(&http.Client{Transport: rt}),
			httpclient.WithoutRetry(),
			httpclient.WithCircuitBreakerSettings("svc.test", &httpclient.CircuitBreakerSettings{
				Name:          "svc",
				Timeout:       time.Hour,
				OnStateChange: httpclient.DefaultOnStateChange(logger),
			}),
		)
		for range httpclient.DefaultBreakerMinRequests {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://svc.test/", nil)
			require.NoError(t, err)
			resp, err := c.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			require.Error(t, err)
		}
		require.Contains(t, out.String(), "circuit [svc] change state from closed to open")
		require.Contains(t, out.String(), "level=DEBUG")
	})

	t.Run("nil_logger", func(t *testing.T) {
		t.Parallel()
		require.NotPanics(t, func() {
			httpclient.DefaultOnStateChange(nil)("svc", gobreaker.StateClosed, gobreaker.StateOpen)
		})
	})
}
