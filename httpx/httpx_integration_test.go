//go:build integration

// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package httpx_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/httpx"
)

// TestIntegration_HeaderGetterOnServerRequest reads headers of a request
// received over loopback, with repeated field lines and mixed-case names.
func TestIntegration_HeaderGetterOnServerRequest(t *testing.T) {
	t.Parallel()

	type seen struct {
		all    []string
		single string
		absent []string
	}
	got := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g := httpx.NewHTTPHeaderGetter(r)
		got <- seen{all: g.GetHeader("x-forwarded-for"), single: g.GetSingleHeader("X-FORWARDED-FOR"), absent: g.GetHeader("X-Missing")}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	req.Header["X-Forwarded-For"] = []string{"8.8.8.8, 10.0.0.1", "10.0.0.2"}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	s := <-got
	require.Equal(t, []string{"8.8.8.8, 10.0.0.1", "10.0.0.2"}, s.all, "every field line arrives, in order")
	require.Equal(t, "8.8.8.8, 10.0.0.1", s.single)
	require.Empty(t, s.absent)
}

// TestIntegration_RoundTripperAndDoerFuncs composes the function adapters
// into a real client stack: a RoundTripperFunc middleware over the default
// transport and a DoerFunc decorator around the client.
func TestIntegration_RoundTripperAndDoerFuncs(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Header.Get("X-Request-Id")+"|"+r.Header.Get("User-Agent"))
	}))
	t.Cleanup(srv.Close)

	var trips int
	transport := httpx.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		trips++
		req = req.Clone(req.Context())
		req.Header.Set("X-Request-Id", "req-"+strconv.Itoa(trips))
		return http.DefaultTransport.RoundTrip(req)
	})
	client := &http.Client{Transport: transport}
	var doer httpx.Doer = httpx.DoerFunc(func(req *http.Request) (*http.Response, error) {
		req.Header.Set("User-Agent", "atlas-it")
		return client.Do(req)
	})

	for i := 1; i <= 2; i++ {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
		require.NoError(t, err)
		resp, err := doer.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.NoError(t, err)
		require.Equal(t, "req-"+strconv.Itoa(i)+"|atlas-it", string(body))
	}
	require.Equal(t, 2, trips)
}

// TestIntegration_StatusClassesOfRealReplies classifies real replies with
// redirects left unfollowed.
func TestIntegration_StatusClassesOfRealReplies(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil {
			code = http.StatusBadRequest
		}
		if code >= 300 && code < 400 && code != http.StatusNotModified {
			w.Header().Set("Location", "/200")
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	cases := []struct {
		status       int
		wantSuccess  bool
		wantRedirect bool
	}{
		{http.StatusOK, true, false},
		{http.StatusNoContent, true, false},
		{http.StatusMovedPermanently, false, true},
		{http.StatusNotModified, false, true},
		{http.StatusPermanentRedirect, false, true},
		{http.StatusNotFound, false, false},
		{http.StatusServiceUnavailable, false, false},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/"+strconv.Itoa(tc.status), nil)
			require.NoError(t, err)
			resp, err := client.Do(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			require.Equal(t, tc.status, resp.StatusCode)
			require.Equal(t, tc.wantSuccess, httpx.IsSuccess(resp.StatusCode))
			require.Equal(t, tc.wantRedirect, httpx.IsRedirect(resp.StatusCode))
		})
	}
}
