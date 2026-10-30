// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/http/client/limiters"

	coreio "github.com/altessa-s/go-atlas-core/io"
)

type pinCtxKey struct{}

func TestBuildWithContext_Pin(t *testing.T) {
	t.Parallel()

	t.Run("an empty method defaults to GET", func(t *testing.T) {
		t.Parallel()
		rb := &requestBuilder{url: "http://example.com/p"}
		req, err := rb.BuildWithContext(t.Context())
		require.NoError(t, err)
		require.Equal(t, http.MethodGet, req.Method)
	})

	t.Run("a deferred body error is returned", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("boom")
		rb := &requestBuilder{method: http.MethodPost, url: "http://example.com/p", body: &coreio.ErrorReader{Err: boom}}
		_, err := rb.BuildWithContext(t.Context())
		require.ErrorIs(t, err, boom)
	})

	t.Run("the builder context is used when none is given", func(t *testing.T) {
		t.Parallel()
		ctx := context.WithValue(t.Context(), pinCtxKey{}, "builder")
		rb := &requestBuilder{method: http.MethodGet, url: "http://example.com/p", ctx: ctx}
		req, err := rb.BuildWithContext(nil) //nolint:staticcheck // a nil context selects the builder's
		require.NoError(t, err)
		require.Equal(t, "builder", req.Context().Value(pinCtxKey{}))
	})

	t.Run("an invalid method fails", func(t *testing.T) {
		t.Parallel()
		rb := &requestBuilder{method: "BAD METHOD", url: "http://example.com/p"}
		_, err := rb.BuildWithContext(t.Context())
		require.Error(t, err)
	})

	t.Run("query parameters merge into an existing query", func(t *testing.T) {
		t.Parallel()
		rb := &requestBuilder{method: http.MethodGet, url: "http://example.com/p?a=1", queryParams: url.Values{"b": {"2"}}}
		req, err := rb.BuildWithContext(t.Context())
		require.NoError(t, err)
		require.Equal(t, "a=1&b=2", req.URL.RawQuery)
	})

	t.Run("a malformed existing query fails the merge", func(t *testing.T) {
		t.Parallel()
		rb := &requestBuilder{method: http.MethodGet, url: "http://example.com/p?a=%zz", queryParams: url.Values{"b": {"2"}}}
		_, err := rb.BuildWithContext(t.Context())
		require.Error(t, err)
	})
}

func TestRenderTrusted_Pin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "<nil>"},
		{"retryable status", &retryableStatusError{status: http.StatusServiceUnavailable}, "retryable HTTP status: 503"},
		{"status value", fmt.Errorf("w: %w", UnexpectedStatusError{Status: 404, Method: "GET", Host: "h", URI: "/p?q=1"}),
			"unexpected response status: 404 (Not Found); request: GET h/p"},
		{"rate limit with retry after", &limiters.RateLimitError{Host: "h", RetryAfter: time.Second},
			`rate limit exceeded for host "h", retry after 1s`},
		{"long invalid host", fmt.Errorf("parse: %w", url.InvalidHostError("ab")), "invalid character in host name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, renderTrusted(tc.err))
		})
	}
}
