//go:build integration

// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"
	"github.com/stretchr/testify/require"

	coreio "github.com/altessa-s/go-atlas-core/io"
	"github.com/altessa-s/go-atlas-core/time/clock"

	"github.com/altessa-s/go-atlas-net/http/client/limiters"
	"github.com/altessa-s/go-atlas-net/http/client/limiters/hostpacer"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

// itServer starts a loopback HTTP server and counts the requests it served.
func itServer(tb testing.TB, h func(n int32, w http.ResponseWriter, r *http.Request)) (*httptest.Server, *atomic.Int32) {
	tb.Helper()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h(hits.Add(1), w, r)
	}))
	tb.Cleanup(srv.Close)
	return srv, &hits
}

func TestIntegration_RetryAfterIsHonoredAndCapped(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		arrivals []time.Time
	)
	srv, hits := itServer(t, func(n int32, w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		mu.Unlock()
		if n <= 2 {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})

	const capWait = 80 * time.Millisecond
	c := httpclient.NewHTTPClient(itLoopback, httpclient.WithRetryMax(3),
		httpclient.WithRetryWait(time.Millisecond, time.Millisecond), httpclient.WithRetryJitter(0),
		httpclient.WithRetryAfterMax(capWait))

	start := time.Now()
	status, body, err := itGet(t, c, srv.URL)
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "ok", body)
	require.Equal(t, int32(3), hits.Load())
	require.Less(t, elapsed, 10*time.Second, "Retry-After: 30 must be capped by WithRetryAfterMax")

	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(arrivals); i++ {
		require.GreaterOrEqual(t, arrivals[i].Sub(arrivals[i-1]), capWait, "retry %d came before the honored Retry-After", i)
	}
}

func TestIntegration_AttemptTimeoutRetriesStalledAttempt(t *testing.T) {
	t.Parallel()

	firstCanceled := make(chan struct{})
	srv, hits := itServer(t, func(n int32, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			<-r.Context().Done() // stall until the client abandons the attempt
			close(firstCanceled)
			return
		}
		_, _ = io.WriteString(w, "second")
	})

	c := httpclient.NewHTTPClient(itLoopback, httpclient.WithRetryMax(1),
		httpclient.WithRetryWait(time.Millisecond, time.Millisecond), httpclient.WithAttemptTimeout(150*time.Millisecond))

	status, body, err := itGet(t, c, srv.URL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "second", body)
	require.Equal(t, int32(2), hits.Load())
	select {
	case <-firstCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("the server never saw the stalled attempt end")
	}
}

// itBreakerEvents records breaker transitions and keeps the breakers view.
type itBreakerEvents struct {
	httpclient.NoopObserver

	breakers httpclient.Breakers
	mu       sync.Mutex
	changes  []string
}

func (o *itBreakerEvents) Attach(b httpclient.Breakers) { o.breakers = b }

func (o *itBreakerEvents) BreakerStateChanged(e httpclient.BreakerStateEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.changes = append(o.changes, e.From.String()+"->"+e.To.String())
}

func (o *itBreakerEvents) transitions() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.changes...)
}

// TestIntegration_CircuitBreakerTripsAndRecovers drops connections without a
// reply until the breaker opens, checks that the open breaker keeps requests
// off the network, then lets the server recover and the breaker close.
func TestIntegration_CircuitBreakerTripsAndRecovers(t *testing.T) {
	t.Parallel()

	var healthy atomic.Bool
	srv, hits := itServer(t, func(_ int32, w http.ResponseWriter, _ *http.Request) {
		if healthy.Load() {
			_, _ = io.WriteString(w, "up")
			return
		}
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_ = conn.Close() // no reply at all: a transport failure
		}
	})
	host := strings.TrimPrefix(srv.URL, "http://")

	const breakerTimeout = 200 * time.Millisecond
	events := &itBreakerEvents{}
	c := httpclient.NewHTTPClient(itLoopback, httpclient.WithRetryMax(0),
		httpclient.WithBreakerTimeout(breakerTimeout), httpclient.WithBreakerMaxRequests(1), httpclient.WithObserver(events))

	for range httpclient.DefaultBreakerMinRequests {
		_, _, err := itGet(t, c, srv.URL)
		require.Error(t, err)
		require.False(t, httpclient.IsCircuitBreakerOpen(err))
	}
	require.Equal(t, gobreaker.StateOpen, events.breakers.HostState(host))

	served := hits.Load()
	_, _, err := itGet(t, c, srv.URL)
	require.ErrorIs(t, err, httpclient.ErrCircuitBreakerOpen)
	require.Equal(t, served, hits.Load(), "an open breaker must not reach the server")

	healthy.Store(true)
	time.Sleep(breakerTimeout + 50*time.Millisecond)
	status, body, err := itGet(t, c, srv.URL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "up", body)
	require.Equal(t, gobreaker.StateClosed, events.breakers.HostState(host))
	require.Equal(t, []string{"closed->open", "open->half-open", "half-open->closed"}, events.transitions())
}

func TestIntegration_LimiterRefusalNeverReachesServer(t *testing.T) {
	t.Parallel()

	srv, hits := itServer(t, func(_ int32, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	errQuota := errors.New("quota exhausted")
	var keys []string
	c := httpclient.NewHTTPClient(itLoopback, httpclient.WithRetryMax(3),
		httpclient.WithLimiter(limiters.Func(func(_ context.Context, key string) error {
			keys = append(keys, key)
			return errQuota
		})))

	_, _, err := itGet(t, c, srv.URL)
	require.ErrorIs(t, err, limiters.ErrRateLimited)
	require.ErrorIs(t, err, errQuota)
	rle := limiters.IsRateLimitError(err)
	require.NotNil(t, rle)
	require.Equal(t, "127.0.0.1", rle.Host)
	require.Equal(t, []string{"127.0.0.1"}, keys)
	require.Zero(t, hits.Load())
}

// TestIntegration_HostPacerPacesEveryAttempt runs real requests through a
// per-attempt host pacer on a fake clock: every attempt, retries included,
// waits for its slot, and the waits add up to the configured interval.
func TestIntegration_HostPacerPacesEveryAttempt(t *testing.T) {
	t.Parallel()

	srv, hits := itServer(t, func(n int32, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	fake := clock.NewFake(start)
	const interval = time.Second
	pacer, err := hostpacer.New(interval, 1, hostpacer.WithClock(fake))
	require.NoError(t, err)
	c := httpclient.NewHTTPClient(itLoopback, httpclient.WithRetryMax(1), httpclient.WithRetryWait(time.Nanosecond, time.Nanosecond),
		httpclient.WithRetryJitter(0), httpclient.WithClock(fake), httpclient.WithLimiter(pacer), httpclient.WithLimiterPerAttempt())

	for range 2 {
		status, _, err := itGet(t, c, srv.URL)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, status)
	}
	require.Equal(t, int32(3), hits.Load())
	require.Equal(t, 1, pacer.Len())
	require.InDelta(t, float64(2*interval), float64(fake.Now().Sub(start)), float64(time.Millisecond),
		"three attempts at one per second wait two intervals")
}

func TestIntegration_RedirectsAndCookieJar(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "s-42", Path: "/"})
		http.Redirect(w, r, "/home", http.StatusFound)
	})
	mux.HandleFunc("/home", func(w http.ResponseWriter, r *http.Request) {
		ck, err := r.Cookie("session")
		if err != nil {
			http.Error(w, "no session", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "welcome "+ck.Value)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	t.Run("follows_redirect_with_cookie", func(t *testing.T) {
		t.Parallel()

		jar, err := cookiejar.New(nil)
		require.NoError(t, err)
		c := httpclient.NewHTTPClient(itLoopback, httpclient.WithRetryMax(0), httpclient.WithCookieJar(jar))

		status, body, err := itGet(t, c, srv.URL+"/login")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "welcome s-42", body, "the cookie of the redirect reply reaches the next hop")
	})

	t.Run("without_redirects_still_stores_cookie", func(t *testing.T) {
		t.Parallel()

		jar, err := cookiejar.New(nil)
		require.NoError(t, err)
		c := httpclient.NewHTTPClient(itLoopback, httpclient.WithRetryMax(0), httpclient.WithCookieJar(jar), httpclient.WithoutRedirects())

		resp, err := c.Get(t.Context(), srv.URL+"/login")
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusFound, resp.StatusCode)
		require.Equal(t, "/home", resp.Header.Get("Location"))

		u, err := url.Parse(srv.URL)
		require.NoError(t, err)
		require.Len(t, jar.Cookies(u), 1, "the 302 reply's cookie is stored although it is not followed")

		status, body, err := itGet(t, c, srv.URL+"/home")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "welcome s-42", body)
	})
}

// TestIntegration_BodyReplayedOnRetry checks that a retried request sends the
// whole body again, and that a POST without an idempotency key is sent once.
func TestIntegration_BodyReplayedOnRetry(t *testing.T) {
	t.Parallel()

	payload := make([]byte, 256<<10)
	_, err := rand.Read(payload)
	require.NoError(t, err)
	want := sha256.Sum256(payload)

	cases := []struct {
		name     string
		header   http.Header
		wantHits int32
		wantErr  error
	}{
		{name: "idempotency_key", header: http.Header{"Idempotency-Key": {"order-1"}}, wantHits: 2},
		{name: "plain_post", header: http.Header{}, wantHits: 1, wantErr: httpclient.ErrMaxRetriesExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var mismatches atomic.Int32
			srv, hits := itServer(t, func(n int32, w http.ResponseWriter, r *http.Request) {
				got, rerr := io.ReadAll(r.Body)
				if rerr != nil || sha256.Sum256(got) != want {
					mismatches.Add(1)
				}
				if n == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusCreated)
			})
			c := httpclient.New(itLoopback, httpclient.WithRetryMax(2), httpclient.WithRetryWait(time.Millisecond, time.Millisecond))

			// Hide the reader type so the body cannot be rewound by net/http.
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, struct{ io.Reader }{bytes.NewReader(payload)})
			require.NoError(t, err)
			req.Header = tc.header
			resp, err := c.Do(req)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				_ = resp.Body.Close()
				require.Equal(t, http.StatusCreated, resp.StatusCode)
			}
			require.Equal(t, tc.wantHits, hits.Load())
			require.Zero(t, mismatches.Load(), "every attempt must carry the full body")
		})
	}
}

func TestIntegration_CancelWhileReadingBody(t *testing.T) {
	t.Parallel()

	handlerDone := make(chan struct{})
	srv, _ := itServer(t, func(_ int32, w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "first chunk;")
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
	})
	c := httpclient.NewHTTPClient(itLoopback, httpclient.WithRetryMax(0))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resp, err := c.Get(ctx, srv.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	buf := make([]byte, len("first chunk;"))
	_, err = io.ReadFull(resp.Body, buf)
	require.NoError(t, err)
	require.Equal(t, "first chunk;", string(buf))

	cancel()
	_, err = io.ReadAll(resp.Body)
	require.ErrorIs(t, err, context.Canceled)
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("canceling the body read did not release the server")
	}
}

func TestIntegration_ResponseSizeLimit(t *testing.T) {
	t.Parallel()

	const limit = 1 << 10
	big := strings.Repeat("z", 2*limit)
	mux := http.NewServeMux()
	var hits atomic.Int32
	mux.HandleFunc("/sized", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", strconv.Itoa(len(big)))
		_, _ = io.WriteString(w, big)
	})
	mux.HandleFunc("/chunked", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		for range 8 {
			_, _ = io.WriteString(w, big[:limit/4])
			_ = http.NewResponseController(w).Flush()
		}
	})
	mux.HandleFunc("/small", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, big[:limit])
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := httpclient.NewHTTPClient(itLoopback, httpclient.WithRetryMax(3), httpclient.WithMaxResponseSize(limit))

	t.Run("content_length_over_limit", func(t *testing.T) {
		before := hits.Load()
		_, _, err := itGet(t, c, srv.URL+"/sized")
		require.ErrorIs(t, err, httpclient.ErrResponseSizeExceeded)
		rse := httpclient.IsResponseSizeError(err)
		require.NotNil(t, rse)
		require.Equal(t, int64(limit), rse.Limit)
		require.Equal(t, int64(len(big)), rse.Size)
		require.Equal(t, http.StatusOK, rse.Status)
		require.Equal(t, int32(1), hits.Load()-before, "an oversized reply is never retried")
	})

	t.Run("chunked_over_limit", func(t *testing.T) {
		resp, err := c.Get(t.Context(), srv.URL+"/chunked")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		_, err = io.ReadAll(resp.Body)
		require.ErrorIs(t, err, coreio.ErrReadLimitExceeded)
	})

	t.Run("at_limit", func(t *testing.T) {
		status, body, err := itGet(t, c, srv.URL+"/small")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, status)
		require.Len(t, body, limit)
	})
}

// TestIntegration_SSRFAndEgressPolicy points clients with different policies
// at real loopback servers.
func TestIntegration_SSRFAndEgressPolicy(t *testing.T) {
	t.Parallel()

	allowed, allowedHits := itServer(t, func(_ int32, w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "allowed") })
	other, otherHits := itServer(t, func(_ int32, w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "other") })
	allowedPort := uint16(allowed.Listener.Addr().(*net.TCPAddr).Port) // #nosec G115 -- a TCP port fits
	localhostURL := "http://localhost:" + strconv.Itoa(int(allowedPort))

	permitOne, err := httpclient.NewEgressPolicy(
		[]httpclient.EgressRule{{CIDR: netip.MustParsePrefix("127.0.0.1/32"), Port: allowedPort}}, nil)
	require.NoError(t, err)
	denyLocalhost, err := httpclient.NewEgressPolicy(
		[]httpclient.EgressRule{{CIDR: netip.MustParsePrefix("127.0.0.0/8")}, {CIDR: netip.MustParsePrefix("::1/128")}},
		[]httpclient.EgressRule{{Name: "noLocalhost", Host: "localhost"}})
	require.NoError(t, err)

	cases := []struct {
		name     string
		opts     []httpclient.Option
		url      string
		wantBody string
		wantRule string
	}{
		{name: "default_blocks_loopback_ip", url: allowed.URL},
		{name: "default_blocks_loopback_name", url: localhostURL},
		{name: "allowed_cidr", opts: []httpclient.Option{itLoopback}, url: allowed.URL, wantBody: "allowed"},
		{name: "egress_allow_rule", opts: []httpclient.Option{httpclient.WithEgressPolicy(permitOne)}, url: allowed.URL, wantBody: "allowed"},
		{name: "egress_allow_rule_other_port", opts: []httpclient.Option{httpclient.WithEgressPolicy(permitOne)}, url: other.URL},
		{name: "egress_deny_rule_wins", opts: []httpclient.Option{httpclient.WithEgressPolicy(denyLocalhost)}, url: localhostURL, wantRule: "noLocalhost host=localhost"},
		{name: "egress_allows_ip_not_denied", opts: []httpclient.Option{httpclient.WithEgressPolicy(denyLocalhost)}, url: other.URL, wantBody: "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := httpclient.NewHTTPClient(append([]httpclient.Option{httpclient.WithRetryMax(2)}, tc.opts...)...)
			status, body, err := itGet(t, c, tc.url)
			if tc.wantBody != "" {
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, status)
				require.Equal(t, tc.wantBody, body)
				return
			}
			require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)
			ssrf := httpclient.IsSSRFError(err)
			require.NotNil(t, ssrf)
			require.Equal(t, tc.wantRule, ssrf.Rule)
		})
	}
	t.Cleanup(func() {
		// Only the cases expecting a body may have reached a server.
		require.Equal(t, int32(2), allowedHits.Load())
		require.Equal(t, int32(1), otherHits.Load())
	})
}
