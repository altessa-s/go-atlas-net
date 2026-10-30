// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package client_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/sony/gobreaker/v2"
	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/http/client/limiters"
	"github.com/altessa-s/go-atlas-net/httpx"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

func TestDefaultRetryableStatuses(t *testing.T) {
	t.Parallel()

	got := httpclient.DefaultRetryableStatuses()
	require.Equal(t, []int{
		http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout,
	}, got)
	got[0] = http.StatusTeapot
	require.Equal(t, http.StatusRequestTimeout, httpclient.DefaultRetryableStatuses()[0], "every call returns a fresh slice")
}

func TestDefaultRetryPolicyTransportErrors(t *testing.T) {
	t.Parallel()

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	generic := errors.New("connection reset")
	// The error net/http's Transport returns for a scheme it does not support,
	// bare, as a round tripper receives it.
	ftp := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "ftp://example.org/file", nil)
	resp, schemeErr := (&http.Transport{}).RoundTrip(ftp) //nolint:bodyclose // the round trip fails without a reply
	require.Nil(t, resp)
	require.Error(t, schemeErr)
	verifyErr := &tls.CertificateVerificationError{Err: errors.New("custom verification failed")}
	invalidErr := x509.CertificateInvalidError{Reason: x509.Expired}
	tests := []struct {
		name  string
		ctx   context.Context
		err   error
		retry bool
		is    error
	}{
		{name: "transient", ctx: t.Context(), err: generic, retry: true, is: generic},
		{name: "context ended", ctx: canceled, err: generic, is: context.Canceled},
		{name: "typed SSRF refusal", ctx: t.Context(), err: &httpclient.SSRFError{Host: "h", IP: "10.0.0.1"}, is: httpclient.ErrSSRFBlocked},
		{name: "sentinel-only SSRF refusal", ctx: t.Context(), err: fmt.Errorf("policy: %w", httpclient.ErrSSRFBlocked), is: httpclient.ErrSSRFBlocked},
		{name: "limiter refusal", ctx: t.Context(), err: &limiters.RateLimitError{Host: "h"}, is: limiters.ErrRateLimited},
		{name: "oversized reply", ctx: t.Context(), err: &httpclient.ResponseSizeError{Limit: 1, Size: 2}, is: httpclient.ErrResponseSizeExceeded},
		{name: "open breaker", ctx: t.Context(), err: fmt.Errorf("execute: %w", gobreaker.ErrOpenState), is: httpclient.ErrCircuitBreakerOpen},
		{name: "unknown authority", ctx: t.Context(), err: x509.UnknownAuthorityError{}, is: x509.UnknownAuthorityError{}},
		{name: "certificate verification", ctx: t.Context(), err: verifyErr, is: verifyErr},
		{name: "host name mismatch", ctx: t.Context(), err: x509.HostnameError{Host: "h"}, is: x509.HostnameError{Host: "h"}},
		{name: "invalid certificate", ctx: t.Context(), err: invalidErr, is: invalidErr},
		{name: "unsupported scheme", ctx: t.Context(), err: schemeErr, is: schemeErr},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			retry, err := httpclient.DefaultRetryPolicy(tc.ctx, nil, tc.err)
			require.Equal(t, tc.retry, retry)
			require.ErrorIs(t, err, tc.is)
			require.NotErrorIs(t, err, httpclient.ErrNonRetryable, "the handler protocol marks (false, err) itself")
		})
	}
}

func TestDefaultRetryPolicyReplies(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://api.test/v1/items?token=secret", nil)
	tests := []struct {
		name   string
		resp   *http.Response
		retry  bool
		status int // non-zero: an UnexpectedStatusError with this status
	}{
		{name: "ok", resp: &http.Response{StatusCode: http.StatusOK, Request: req}},
		{name: "redirect", resp: &http.Response{StatusCode: http.StatusFound, Request: req}},
		{name: "unavailable", resp: &http.Response{StatusCode: http.StatusServiceUnavailable, Request: req}, retry: true},
		{name: "too many requests", resp: &http.Response{StatusCode: http.StatusTooManyRequests, Request: req}, retry: true},
		{name: "internal error", resp: &http.Response{StatusCode: http.StatusInternalServerError, Request: req}, status: 500},
		{name: "not found", resp: &http.Response{StatusCode: http.StatusNotFound, Request: req}, status: 404},
		{name: "no status", resp: &http.Response{Request: req}, status: -1},
		{name: "no request", resp: &http.Response{StatusCode: http.StatusBadRequest}, status: 400},
		{name: "no reply"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			retry, err := httpclient.DefaultRetryPolicy(t.Context(), tc.resp, nil)
			require.Equal(t, tc.retry, retry)
			if tc.status == 0 {
				require.NoError(t, err)
				return
			}
			se := httpclient.IsUnexpectedStatusError(err)
			require.NotNil(t, se, "%v", err)
			require.Equal(t, max(tc.status, 0), se.Status)
			if tc.resp.Request != nil {
				require.Equal(t, "api.test", se.Host)
				require.Equal(t, "/v1/items", se.URI, "the query string is omitted")
			}
		})
	}
}

// A client using DefaultRetryPolicy as its handler classifies like the
// built-in policy.
func TestDefaultRetryPolicyAsHandler(t *testing.T) {
	t.Parallel()

	unavailable, hits := statusServer(t, http.StatusServiceUnavailable)
	c := httpclient.New(loopback, httpclient.WithRetryMax(2), fastRetries, httpclient.WithRetryPolicyHandler(httpclient.DefaultRetryPolicy))
	err := doGet(t, c, t.Context(), unavailable.URL)
	require.EqualValues(t, 3, hits.Load())
	require.ErrorIs(t, err, httpclient.ErrMaxRetriesExceeded)
	require.Equal(t, http.StatusServiceUnavailable, httpclient.IsUnexpectedStatusError(err).Status)

	notFound, hits := statusServer(t, http.StatusNotFound)
	err = doGet(t, c, t.Context(), notFound.URL)
	require.EqualValues(t, 1, hits.Load())
	require.Equal(t, http.StatusNotFound, httpclient.IsUnexpectedStatusError(err).Status)
}

// flakyServer answers failures times with status, then 200, counting hits.
func flakyServer(t *testing.T, status int, failures int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) <= failures {
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestWithRetryableStatuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		statuses []int
		status   int
		hits     int32
		failed   bool
	}{
		{name: "default does not retry 500", status: http.StatusInternalServerError, hits: 1, failed: true},
		{name: "500 added", statuses: []int{http.StatusInternalServerError, http.StatusServiceUnavailable}, status: http.StatusInternalServerError, hits: 3},
		{name: "the set is replaced", statuses: []int{http.StatusInternalServerError}, status: http.StatusServiceUnavailable, hits: 1, failed: true},
		{name: "empty keeps the default", statuses: []int{}, status: http.StatusServiceUnavailable, hits: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv, hits := flakyServer(t, tc.status, 2)
			opts := []httpclient.Option{loopback, httpclient.WithRetryMax(3), fastRetries}
			if tc.statuses != nil {
				opts = append(opts, httpclient.WithRetryableStatuses(tc.statuses...))
			}
			err := doGet(t, httpclient.New(opts...), t.Context(), srv.URL)
			require.Equal(t, tc.hits, hits.Load())
			if !tc.failed {
				require.NoError(t, err)
				return
			}
			se := httpclient.IsUnexpectedStatusError(err)
			require.NotNil(t, se, "%v", err)
			require.Equal(t, tc.status, se.Status)
			require.Nil(t, httpclient.IsRetryExhaustedError(err))
		})
	}
}

// An SSRF refusal is never retried, whether it is the typed error or only
// matches the sentinel.
func TestSSRFRefusalNotRetried(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{name: "typed", err: &httpclient.SSRFError{Host: "internal.test", IP: "10.0.0.1"}},
		{name: "wrapped typed", err: fmt.Errorf("dial: %w", &httpclient.SSRFError{Host: "internal.test", IP: "10.0.0.1"})},
		{name: "sentinel only", err: fmt.Errorf("egress policy denies 10.0.0.1: %w", httpclient.ErrSSRFBlocked)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var calls atomic.Int32
			refusing := httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return nil, tc.err
			})
			c := httpclient.New(httpclient.WithoutSSRFProtection(), httpclient.WithClient(&http.Client{Transport: refusing}),
				httpclient.WithRetryMax(3), fastRetries)
			err := doGet(t, c, t.Context(), "http://internal.test/x")
			require.EqualValues(t, 1, calls.Load())
			require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)
			require.ErrorIs(t, err, httpclient.ErrNonRetryable)
			require.ErrorIs(t, err, tc.err)
			var urlErr *url.Error
			require.ErrorAs(t, err, &urlErr)
		})
	}
}

// A server certificate the client rejects fails the request after a single
// attempt: the bare TLS error a round tripper receives is not retried, so the
// handshake is not repeated (nor does it count towards the breaker again).
func TestRetryRejectedCertificateIsNotRetried(t *testing.T) {
	t.Parallel()

	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the request reached the handler over an untrusted certificate")
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the server logs every failed handshake
	srv.StartTLS()
	t.Cleanup(srv.Close)

	// The default trust store does not know httptest's certificate.
	c := httpclient.New(loopback, httpclient.WithRetryMax(3), fastRetries)
	err := doGet(t, c, t.Context(), srv.URL+"/x")
	require.Error(t, err)
	_, ok := errors.AsType[*tls.CertificateVerificationError](err)
	require.True(t, ok, "%v", err)
	require.NotErrorIs(t, err, httpclient.ErrMaxRetriesExceeded)
	require.ErrorIs(t, err, httpclient.ErrNonRetryable)
	require.EqualValues(t, 1, conns.Load(), "one TLS handshake, no retry")
}
