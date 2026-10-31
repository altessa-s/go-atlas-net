// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/altessa-s/go-atlas-net/http/client/limiters"
)

func BenchmarkNew(b *testing.B) {
	for b.Loop() {
		New()
	}
}

func BenchmarkNewHTTPClient(b *testing.B) {
	for b.Loop() {
		NewHTTPClient()
	}
}

// BenchmarkHTTPClient_PostBody measures the retry round-tripper cost for
// body-bearing requests: with GetBody available the body must not be
// buffered, and with retries disabled buffering is unnecessary entirely.
func BenchmarkHTTPClient_PostBody(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	payload := bytes.Repeat([]byte("x"), 4096)
	ctx := b.Context()

	b.Run("with_getbody_retries_enabled", func(b *testing.B) {
		c := New(WithSSRFAllowedCIDRs(netip.MustParsePrefix("127.0.0.0/8")), WithRetryMax(2))
		b.ReportAllocs()
		for b.Loop() {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader(payload))
			if err != nil {
				b.Fatal(err)
			}
			resp, err := c.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})

	b.Run("opaque_body_no_retries", func(b *testing.B) {
		c := New(WithSSRFAllowedCIDRs(netip.MustParsePrefix("127.0.0.0/8")), WithRetryMax(0))
		b.ReportAllocs()
		for b.Loop() {
			// Hide the reader type so net/http cannot derive GetBody.
			body := struct{ io.Reader }{bytes.NewReader(payload)}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, body)
			if err != nil {
				b.Fatal(err)
			}
			resp, err := c.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

func BenchmarkHTTPClient_Get(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewHTTPClient(WithSSRFAllowedCIDRs(netip.MustParsePrefix("127.0.0.0/8")), WithRetryMax(0))
	ctx := b.Context()

	for b.Loop() {
		resp, err := c.Get(ctx, srv.URL)
		if err != nil {
			b.Fatalf("Get() error = %v", err)
		}
		resp.Body.Close()
	}
}

// BenchmarkHTTPClient_RetryAfter503 measures an HTTP/1.1 request that first
// gets a small 503 and succeeds on the retry. conns/op reports how many new
// TCP connections the client opened per request: draining the discarded 503
// lets the retry reuse the same connection.
func BenchmarkHTTPClient_RetryAfter503(b *testing.B) {
	var calls, conns atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1)%2 == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	c := New(WithSSRFAllowedCIDRs(netip.MustParsePrefix("127.0.0.0/8")), WithRetryMax(1), WithRetryWait(time.Nanosecond, time.Nanosecond))
	// Discarded bodies are drained only under a deadline.
	ctx, cancel := context.WithTimeout(b.Context(), time.Hour)
	defer cancel()

	b.ReportAllocs()
	for b.Loop() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		if err != nil {
			b.Fatal(err)
		}
		resp, err := c.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	b.ReportMetric(float64(conns.Load())/float64(b.N), "conns/op")
}

// BenchmarkClientStack_RoundTrip measures a GET through the whole client
// stack (retry, breaker, SSRF guard, transport) against a loopback server,
// over plain HTTP/1.1, TLS with HTTP/1.1 and HTTP/2, and with the optional
// layers a service typically enables.
func BenchmarkClientStack_RoundTrip(b *testing.B) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "s", Value: "1"})
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	plain := httptest.NewServer(handler)
	defer plain.Close()
	tls1 := httptest.NewTLSServer(handler)
	defer tls1.Close()
	tls2 := httptest.NewUnstartedServer(handler)
	tls2.EnableHTTP2 = true
	tls2.StartTLS()
	defer tls2.Close()

	tlsTransport := func(srv *httptest.Server, h2 bool) *http.Transport {
		t := DefaultPooledTransport()
		t.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		t.ForceAttemptHTTP2 = h2
		return t
	}
	allowLoopback := WithSSRFAllowedCIDRs(netip.MustParsePrefix("127.0.0.0/8"))
	policy, err := NewEgressPolicy([]EgressRule{{CIDR: netip.MustParsePrefix("127.0.0.0/8")}}, []EgressRule{{Host: "*.internal"}, {Port: 25}})
	if err != nil {
		b.Fatal(err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		b.Fatal(err)
	}

	cases := []struct {
		name      string
		url       string
		wantProto int
		opts      []Option
	}{
		{"http1", plain.URL, 1, []Option{allowLoopback}},
		{"tls_http1", tls1.URL, 1, []Option{allowLoopback, WithTransport(tlsTransport(tls1, false))}},
		{"tls_http2", tls2.URL, 2, []Option{allowLoopback, WithTransport(tlsTransport(tls2, true))}},
		{"http1_limiter", plain.URL, 1, []Option{allowLoopback, WithLimiter(limiters.Func(func(context.Context, string) error { return nil }))}},
		{"http1_egress_policy", plain.URL, 1, []Option{WithEgressPolicy(policy)}},
		{"http1_cookie_jar", plain.URL, 1, []Option{allowLoopback, WithCookieJar(jar)}},
		{"http1_max_response_size", plain.URL, 1, []Option{allowLoopback, WithMaxResponseSize(1 << 20)}},
		{"http1_without_retry", plain.URL, 1, []Option{allowLoopback, WithoutRetry()}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			c := New(append([]Option{WithRetryMax(2)}, tc.opts...)...)
			defer c.CloseIdleConnections()
			ctx := b.Context()
			get := func() {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, tc.url, nil)
				if err != nil {
					b.Fatal(err)
				}
				resp, err := c.Do(req)
				if err != nil {
					b.Fatal(err)
				}
				if resp.ProtoMajor != tc.wantProto {
					b.Fatalf("protocol = %s, want HTTP/%d", resp.Proto, tc.wantProto)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			get() // warm the connection pool

			b.ReportAllocs()
			for b.Loop() {
				get()
			}
		})
	}
}
