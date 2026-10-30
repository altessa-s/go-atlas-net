// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package client_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

// redirectServer answers /start with a 302 to /target and counts the hits
// on /target.
func redirectServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var targetHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "abc", Path: "/"})
		w.Header().Set("Location", "/target")
		w.WriteHeader(http.StatusFound)
		_, _ = io.WriteString(w, "moved")
	})
	mux.HandleFunc("/target", func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		_, _ = io.WriteString(w, "target")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &targetHits
}

func getStart(t *testing.T, c *http.Client, srv *httptest.Server) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/start", nil)
	require.NoError(t, err)
	resp, err := c.Do(req)
	require.NoError(t, err, "a 3xx reply is not an error")
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestWithoutRedirects_ReturnsRedirectResponse(t *testing.T) {
	t.Parallel()

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)

	tests := []struct {
		name string
		opts []httpclient.Option
	}{
		{"default client", nil},
		{"without retry", []httpclient.Option{httpclient.WithoutRetry()}},
		{"explicit proxy override", []httpclient.Option{httpclient.WithoutProxy()}},
		{"per-host breaker", []httpclient.Option{httpclient.WithCircuitBreakerSettings("127.0.0.1", &httpclient.CircuitBreakerSettings{})}},
		{"with cookie jar", []httpclient.Option{httpclient.WithCookieJar(jar)}},
		{"without ssrf protection", []httpclient.Option{httpclient.WithoutSSRFProtection()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv, targetHits := redirectServer(t)

			c := httpclient.New(append([]httpclient.Option{loopback, httpclient.WithoutRedirects()}, tt.opts...)...)
			resp := getStart(t, c, srv)

			require.Equal(t, http.StatusFound, resp.StatusCode)
			require.Equal(t, "/target", resp.Header.Get("Location"))
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, "moved", string(body), "the 3xx body reaches the caller unread")
			require.Zero(t, targetHits.Load(), "the redirect is not followed")
		})
	}
}

func TestWithoutRedirects_DefaultFollows(t *testing.T) {
	t.Parallel()
	srv, targetHits := redirectServer(t)

	resp := getStart(t, httpclient.New(loopback), srv)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, int32(1), targetHits.Load())
}

func TestWithoutRedirects_OverridesSuppliedClient(t *testing.T) {
	t.Parallel()
	srv, targetHits := redirectServer(t)

	errPolicy := errors.New("caller policy")
	supplied := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errPolicy }}

	resp := getStart(t, httpclient.New(loopback, httpclient.WithClient(supplied), httpclient.WithoutRedirects()), srv)

	require.Equal(t, http.StatusFound, resp.StatusCode)
	require.Zero(t, targetHits.Load())

	// The caller's client keeps its own policy.
	require.ErrorIs(t, supplied.CheckRedirect(nil, nil), errPolicy)
}

func TestWithoutRedirects_NewHTTPClient(t *testing.T) {
	t.Parallel()
	srv, targetHits := redirectServer(t)

	c := httpclient.NewHTTPClient(loopback, httpclient.WithoutRedirects())
	resp, err := c.Get(t.Context(), srv.URL+"/start")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusFound, resp.StatusCode)
	loc, err := resp.Location()
	require.NoError(t, err)
	require.Equal(t, "/target", loc.Path)
	require.Zero(t, targetHits.Load())
}
