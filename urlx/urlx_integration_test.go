//go:build integration

// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package urlx_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/urlx"
)

// wireRequest is what the server saw of a request line.
type wireRequest struct {
	escapedPath string
	path        string
	rawQuery    string
}

// startPathRecorder serves 200 and records the request line of every request.
func startPathRecorder(tb testing.TB) (*httptest.Server, func() []wireRequest) {
	tb.Helper()

	var (
		mu   sync.Mutex
		seen []wireRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, wireRequest{escapedPath: r.URL.EscapedPath(), path: r.URL.Path, rawQuery: r.URL.RawQuery})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	tb.Cleanup(srv.Close)
	return srv, func() []wireRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]wireRequest(nil), seen...)
	}
}

func sendGet(tb testing.TB, rawURL string) {
	tb.Helper()

	req, err := http.NewRequestWithContext(tb.Context(), http.MethodGet, rawURL, nil)
	require.NoError(tb, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(tb, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	require.Equal(tb, http.StatusOK, resp.StatusCode)
}

// TestIntegration_JoinedPathsOnTheWire sends requests to URLs built with
// ParseBaseURL and JoinPath and checks the request line the server receives.
func TestIntegration_JoinedPathsOnTheWire(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		base        string
		opts        []urlx.Option
		elem        []string
		wantEscaped string
		wantPath    string
	}{
		{
			name:        "trailing_slash_trimmed",
			base:        "/v1/",
			elem:        []string{"chat/completions"},
			wantEscaped: "/v1/chat/completions",
			wantPath:    "/v1/chat/completions",
		},
		{
			name:        "metacharacters_stay_in_path",
			base:        "/v1",
			elem:        []string{"files", "a b?c=d#frag%41"},
			wantEscaped: "/v1/files/a%20b%3Fc=d%23frag%2541",
			wantPath:    "/v1/files/a b?c=d#frag%41",
		},
		{
			name:        "encoded_slash_in_base_kept",
			base:        "/tenant%2Fone",
			elem:        []string{"items"},
			wantEscaped: "/tenant%2Fone/items",
			wantPath:    "/tenant/one/items",
		},
		{
			name:        "trailing_slash_of_last_element_kept",
			base:        "/api",
			elem:        []string{"dir/", ""},
			wantEscaped: "/api/dir/",
			wantPath:    "/api/dir/",
		},
		{
			name:        "endpoint_mode_keeps_base",
			base:        "/hook/",
			opts:        []urlx.Option{urlx.WithEndpointMode()},
			wantEscaped: "/hook/",
			wantPath:    "/hook/",
		},
		{
			name:        "unicode_segment",
			base:        "/v1",
			elem:        []string{"städte", "東京"},
			wantEscaped: "/v1/st%C3%A4dte/%E6%9D%B1%E4%BA%AC",
			wantPath:    "/v1/städte/東京",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv, seen := startPathRecorder(t)
			base, err := urlx.ParseBaseURL(srv.URL+tc.base, tc.opts...)
			require.NoError(t, err)
			target := base.String()
			if len(tc.elem) > 0 {
				target, err = urlx.JoinPath(base, tc.elem...)
				require.NoError(t, err)
			}
			sendGet(t, target)

			got := seen()
			require.Len(t, got, 1)
			require.Equal(t, tc.wantEscaped, got[0].escapedPath)
			require.Equal(t, tc.wantPath, got[0].path)
			require.Empty(t, got[0].rawQuery, "no element may start a query string")
		})
	}
}

// TestIntegration_RejectedURLsNeverSent checks that URLs refused by the
// helpers fail before any request is made.
func TestIntegration_RejectedURLsNeverSent(t *testing.T) {
	t.Parallel()

	srv, seen := startPathRecorder(t)
	base, err := urlx.ParseBaseURL(srv.URL + "/v1")
	require.NoError(t, err)

	for _, elem := range []string{"..", "a/../../admin", "./x"} {
		_, err := urlx.JoinPath(base, elem)
		require.ErrorIs(t, err, urlx.ErrDotSegment, elem)
	}
	for _, raw := range []string{srv.URL + "/v1?token=x", srv.URL + "/v1#frag", strings.Replace(srv.URL, "http://", "http://user:pw@", 1)} {
		_, err := urlx.ParseBaseURL(raw)
		require.ErrorIs(t, err, urlx.ErrInvalidURL)
	}
	_, err = urlx.ParseBaseURL(srv.URL, urlx.WithAllowedSchemes("https"))
	require.ErrorIs(t, err, urlx.ErrSchemeNotAllowed)
	require.Empty(t, seen())
}

// TestIntegration_CanonicalMatchesServedEndpoint: differently spelled base
// URLs of the same server share a canonical form and reach the same path.
func TestIntegration_CanonicalMatchesServedEndpoint(t *testing.T) {
	t.Parallel()

	srv, seen := startPathRecorder(t)
	lower, err := urlx.ParseBaseURL(srv.URL + "/v1/")
	require.NoError(t, err)
	upper, err := urlx.ParseBaseURL(strings.Replace(srv.URL, "http://", "HTTP://", 1) + "/v1")
	require.NoError(t, err)
	require.Equal(t, urlx.Canonical(lower), urlx.Canonical(upper))

	for _, base := range []string{lower.String(), upper.String()} {
		sendGet(t, base)
	}
	got := seen()
	require.Len(t, got, 2)
	require.Equal(t, got[0], got[1])
}
