// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package client_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"
	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-core/errors/network"

	"github.com/altessa-s/go-atlas-net/http/client/limiters"
	"github.com/altessa-s/go-atlas-net/httpx"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

const (
	querySecret    = "redaction-query-secret"
	passwordSecret = "redaction-password"
)

// syncBuffer is a log sink safe for the concurrent writes of a client.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func debugLogger(sink io.Writer) httpclient.Option {
	return httpclient.WithLogger(slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

// secretURL adds credentials and a query secret to base.
func secretURL(base string) string {
	return strings.Replace(base, "://", "://user:"+passwordSecret+"@", 1) + "/orders?token=" + querySecret
}

// roundTripError returns the error the client's retry layer returned: the
// Err of the *url.Error net/http wraps it in. That wrapper carries the
// request URL by design of net/http; the layers below must not.
func roundTripError(t *testing.T, err error) error {
	t.Helper()
	require.Error(t, err)
	var urlErr *url.Error
	require.ErrorAs(t, err, &urlErr)
	return urlErr.Err
}

// requireRedacted fails when a secret of secretURL reaches the error text or
// the logs.
func requireRedacted(t *testing.T, err error, logs string) {
	t.Helper()
	for _, secret := range []string{querySecret, passwordSecret} {
		require.NotContains(t, err.Error(), secret)
		require.NotContains(t, fmt.Sprintf("%+v", err), secret)
		require.NotContains(t, logs, secret)
	}
}

// (a) A failed status, retried or not, never carries the query string.
func TestRedactionStatusError(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusBadRequest, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("token") != querySecret {
					t.Error("the request query was changed")
				}
				w.WriteHeader(status)
			}))
			t.Cleanup(srv.Close)
			var logs syncBuffer
			c := httpclient.New(loopback, httpclient.WithRetryMax(1), fastRetries, debugLogger(&logs))
			err := roundTripError(t, doGet(t, c, t.Context(), secretURL(srv.URL)))
			requireRedacted(t, err, logs.String())

			se := httpclient.IsUnexpectedStatusError(err)
			require.NotNil(t, se, "%v", err)
			require.Equal(t, status, se.Status)
			require.Equal(t, "/orders", se.URI)
			if status == http.StatusServiceUnavailable {
				re := httpclient.IsRetryExhaustedError(err)
				require.NotNil(t, re)
				require.Equal(t, srv.URL+"/orders", re.URL)
				require.Contains(t, logs.String(), "http client retrying request")
			}
		})
	}
}

// failingDialer fails every dial with err.
type failingDialer struct{ err error }

func (d failingDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, d.err
}

// (b) and (d): a dial or resolver failure whose text embeds the query is
// redacted in the returned error and in the retry log, and stays
// classifiable.
func TestRedactionTransportError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		cause error
	}{
		{name: "dns", cause: &net.DNSError{Err: "lookup failed for ?token=" + querySecret, Name: "example.org", IsTemporary: true}},
		{name: "dial", cause: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("proxy refused ?token=" + querySecret)}},
		{name: "url error", cause: &url.Error{Op: "Get", URL: secretURL("http://example.org"), Err: io.ErrUnexpectedEOF}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tr := httpclient.DefaultPooledTransport()
			tr.DialContext = failingDialer{tc.cause}.DialContext
			var logs syncBuffer
			c := httpclient.New(httpclient.WithoutSSRFProtection(), httpclient.WithTransport(tr),
				httpclient.WithRetryMax(1), fastRetries, debugLogger(&logs))
			err := roundTripError(t, doGet(t, c, t.Context(), secretURL("http://example.org")))
			requireRedacted(t, err, logs.String())
			require.Contains(t, logs.String(), "http client retrying request")
			require.ErrorIs(t, err, tc.cause)
			require.ErrorIs(t, err, httpclient.ErrMaxRetriesExceeded)
		})
	}
}

// (c) The circuit breaker's own failure wrapper and its open-state refusal
// do not carry the query string.
func TestRedactionCircuitBreakerError(t *testing.T) {
	t.Parallel()

	failing := httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("upstream reset")
	})
	c := httpclient.New(httpclient.WithoutSSRFProtection(), httpclient.WithClient(&http.Client{Transport: failing}),
		httpclient.WithoutRetry(),
		httpclient.WithCircuitBreakerSettings("example.org", &httpclient.CircuitBreakerSettings{
			MaxRequests: 1,
			ReadyToTrip: func(counts gobreaker.Counts) bool { return counts.ConsecutiveFailures >= 1 },
		}))
	target := secretURL("http://example.org")

	err := roundTripError(t, doGet(t, c, t.Context(), target))
	requireRedacted(t, err, "")
	require.NotErrorIs(t, err, httpclient.ErrCircuitBreakerOpen)

	err = roundTripError(t, doGet(t, c, t.Context(), target))
	requireRedacted(t, err, "")
	require.ErrorIs(t, err, gobreaker.ErrOpenState)
}

// timeoutError is a timeout whose text embeds the query.
type timeoutError struct{}

func (timeoutError) Error() string   { return "deadline for ?token=" + querySecret }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// The redacted error keeps reporting a timeout to *url.Error, which asks its
// Err directly.
func TestRedactionKeepsTimeout(t *testing.T) {
	t.Parallel()

	timingOut := httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, timeoutError{} })
	c := httpclient.New(httpclient.WithoutSSRFProtection(), httpclient.WithClient(&http.Client{Transport: timingOut}),
		httpclient.WithoutRetry())
	err := doGet(t, c, t.Context(), secretURL("http://example.org"))
	var urlErr *url.Error
	require.ErrorAs(t, err, &urlErr)
	require.True(t, urlErr.Timeout())
	require.True(t, urlErr.Temporary()) //nolint:staticcheck // the deprecated method still has callers
	requireRedacted(t, urlErr.Err, "")
	require.Equal(t, "i/o timeout", urlErr.Err.Error())
}

// leakURL is a request URL carrying both secrets.
const leakURL = "http://user:" + passwordSecret + "@example.org/orders?token=" + querySecret

// stringAddr is a net.Addr whose text is set by a custom transport.
type stringAddr string

func (a stringAddr) Network() string { return "tcp" }
func (a stringAddr) String() string  { return string(a) }

// The redacted message keeps the trusted details of a transport failure.
func TestRedactionMessages(t *testing.T) {
	t.Parallel()

	leak := errors.New("custom failure for ?token=" + querySecret)
	tests := []struct {
		name  string
		cause error
		want  string
	}{
		{name: "ssrf", cause: fmt.Errorf("guard: %w", &httpclient.SSRFError{Host: "internal.test", IP: "10.0.0.1"}),
			want: "ssrf: connection to private/local address blocked: host=internal.test ip=10.0.0.1"},
		{name: "ssrf sentinel", cause: fmt.Errorf("policy %s: %w", querySecret, httpclient.ErrSSRFBlocked), want: httpclient.ErrSSRFBlocked.Error()},
		{name: "response size", cause: &httpclient.ResponseSizeError{Limit: 1, Size: 2, Status: 200},
			want: "response size 2 exceeds limit 1 (HTTP status 200)"},
		{name: "rate limit", cause: &limiters.RateLimitError{Host: "example.org", Err: leak},
			want: `rate limit exceeded for host "example.org": HTTP transport error (*errors.errorString)`},
		{name: "breaker", cause: fmt.Errorf("execute %s: %w", querySecret, gobreaker.ErrTooManyRequests), want: gobreaker.ErrTooManyRequests.Error()},
		{name: "canceled", cause: fmt.Errorf("read %s: %w", querySecret, context.Canceled), want: "context canceled"},
		{name: "dns not found", cause: &net.DNSError{Err: querySecret, Name: "example.org", IsNotFound: true},
			want: "lookup example.org: no such host"},
		{name: "unknown authority", cause: fmt.Errorf("handshake %s: %w", querySecret, x509.UnknownAuthorityError{}),
			want: "x509: certificate signed by unknown authority"},
		{name: "refused", cause: &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443},
			Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}, want: "dial tcp 192.0.2.1:443: connection refused"},
		{name: "op without errno", cause: &net.OpError{Op: "read", Net: "tcp", Err: leak}, want: "read tcp: failed"},
		{name: "errno", cause: fmt.Errorf("write %s: %w", querySecret, syscall.ECONNRESET), want: syscall.ECONNRESET.Error()},
		{name: "unexpected EOF", cause: fmt.Errorf("body %s: %w", querySecret, io.ErrUnexpectedEOF), want: "unexpected EOF"},
		{name: "scheme", cause: errors.New(`unsupported protocol scheme "ftp"`), want: "unsupported protocol scheme"},
		{name: "redirect limit", cause: errors.New("stopped after 10 redirects"), want: "stopped after 10 redirects"},
		{name: "redirect limit lookalike", cause: errors.New("policy ?token=" + querySecret + " stopped after 3 redirects"),
			want: "HTTP transport error (*errors.errorString)"},
		{name: "unknown", cause: fmt.Errorf("wrapped: %w", leak), want: "HTTP transport error (*errors.errorString)"},
		// A custom transport may put a whole URL into the identity fields of the typed errors.
		{name: "dns name", cause: &net.DNSError{Name: leakURL, IsTemporary: true}, want: "lookup http:: lookup failed"},
		{name: "op fields", cause: &net.OpError{Op: leakURL, Net: leakURL, Addr: stringAddr(leakURL), Err: syscall.ECONNREFUSED},
			want: "? ? http:: connection refused"},
		{name: "record header", cause: tls.RecordHeaderError{Msg: leakURL}, want: "tls: invalid record header"},
		{name: "hostname", cause: x509.HostnameError{Host: leakURL}, want: "x509: certificate is not valid for http:"},
		{name: "invalid certificate", cause: x509.CertificateInvalidError{Detail: leakURL}, want: "x509: certificate is not valid"},
		{name: "breaker fields", cause: &httpclient.CircuitBreakerError{Name: leakURL, State: leakURL},
			want: `circuit breaker "(unnamed)" is unavailable`},
		{name: "breaker", cause: &httpclient.CircuitBreakerError{Name: "httpclient-example.org", State: "open"},
			want: `circuit breaker "httpclient-example.org" is open`},
		{name: "escape", cause: url.EscapeError("%zz"), want: `invalid URL escape "%zz"`},
		{name: "escape forged", cause: url.EscapeError(leakURL), want: "invalid URL escape"},
		{name: "invalid host forged", cause: url.InvalidHostError(leakURL), want: "invalid character in host name"},
		{name: "unknown authority name", cause: x509.UnknownAuthorityError{Cert: &x509.Certificate{}},
			want: "x509: certificate signed by unknown authority"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			failing := httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, tc.cause })
			c := httpclient.New(httpclient.WithoutSSRFProtection(), httpclient.WithClient(&http.Client{Transport: failing}),
				httpclient.WithoutRetry())
			err := roundTripError(t, doGet(t, c, t.Context(), secretURL("http://example.org")))
			require.ErrorIs(t, err, tc.cause)
			requireRedacted(t, err, "")
			require.Equal(t, tc.want, err.Error())
		})
	}
}

// TestHTTPClientRedactsURLErrors pins that the HTTPClient wrappers and the
// request builder never quote a request URL's query string or user info,
// including in the *url.Error that net/http.Client.Do wraps failures in.
func TestHTTPClientRedactsURLErrors(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	secretURL := "http://user:" + passwordSecret + "@" + addr + "/path?token=" + querySecret

	newClient := func() httpclient.HTTPClient {
		return httpclient.NewHTTPClient(httpclient.WithoutRetry(), httpclient.WithoutSSRFProtection())
	}

	tests := []struct {
		name string
		call func(ctx context.Context) error
	}{
		{"get dial failure", func(ctx context.Context) error {
			resp, err := newClient().Get(ctx, secretURL)
			if resp != nil {
				_ = resp.Body.Close()
			}
			return err
		}},
		{"get invalid url", func(ctx context.Context) error {
			resp, err := newClient().Get(ctx, "http://"+addr+"/%zz?token="+querySecret)
			if resp != nil {
				_ = resp.Body.Close()
			}
			return err
		}},
		{"builder dial failure", func(ctx context.Context) error {
			resp, err := httpclient.NewRequestBuilder(newClient()).GET(secretURL).Send(ctx)
			if resp != nil {
				_ = resp.Body.Close()
			}
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.call(t.Context())
			require.Error(t, err)
			require.NotContains(t, err.Error(), querySecret)
			require.NotContains(t, err.Error(), passwordSecret)
		})
	}
}

// errPolicy is a retry policy's own sentinel, which must stay classifiable.
var errPolicy = errors.New("refused by policy")

// An error a RetryPolicyHandler returns may quote the request URL: it is
// redacted before the retry layer logs it, stores it as the last error of a
// RetryExhaustedError or returns it, and keeps its cause.
func TestRedactionRetryPolicyError(t *testing.T) {
	t.Parallel()

	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprintf("retry=%v", retry), func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			t.Cleanup(srv.Close)
			policy := func(_ context.Context, resp *http.Response, err error) (bool, error) {
				if err != nil {
					return false, err
				}
				return retry, fmt.Errorf("policy for %s: %w", resp.Request.URL, errPolicy)
			}
			var logs syncBuffer
			c := httpclient.New(loopback, httpclient.WithRetryMax(1), fastRetries, debugLogger(&logs),
				httpclient.WithRetryPolicyHandler(policy))
			err := roundTripError(t, doGet(t, c, t.Context(), secretURL(srv.URL)))
			requireRedacted(t, err, logs.String())
			require.ErrorIs(t, err, errPolicy)
			if retry {
				require.Contains(t, logs.String(), "http client retrying request")
				require.ErrorIs(t, err, httpclient.ErrMaxRetriesExceeded)
			} else {
				require.ErrorIs(t, err, httpclient.ErrNonRetryable)
			}
		})
	}
}

// failingReader fails every read with an error quoting the request URL.
type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

// A failure to buffer the request body for retries, or to rewind it for a
// retry, is redacted too.
func TestRedactionRequestBodyError(t *testing.T) {
	t.Parallel()

	leak := errors.New("body source " + secretURL("http://example.org"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	tests := []struct {
		name    string
		request func(t *testing.T) *http.Request
	}{
		{name: "buffer", request: func(t *testing.T) *http.Request {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, secretURL(srv.URL), failingReader{leak})
			require.NoError(t, err)
			return req
		}},
		{name: "rewind", request: func(t *testing.T) *http.Request {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, secretURL(srv.URL), strings.NewReader("payload"))
			require.NoError(t, err)
			calls := 0
			req.GetBody = func() (io.ReadCloser, error) {
				calls++
				if calls > 1 {
					return nil, leak
				}
				return io.NopCloser(strings.NewReader("payload")), nil
			}
			return req
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var logs syncBuffer
			c := httpclient.New(loopback, httpclient.WithRetryMax(2), fastRetries, debugLogger(&logs))
			resp, err := c.Do(tc.request(t))
			if resp != nil {
				_ = resp.Body.Close()
			}
			err = roundTripError(t, err)
			requireRedacted(t, err, logs.String())
			require.ErrorIs(t, err, leak)
		})
	}
}

// RedactError strips the request URL secrets from the *url.Error of the
// *http.Client that New returns, keeps the causes and is idempotent.
func TestRedactError(t *testing.T) {
	t.Parallel()

	failing := httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("reset: %w", syscall.ECONNRESET)
	})
	c := httpclient.New(httpclient.WithoutSSRFProtection(), httpclient.WithClient(&http.Client{Transport: failing}),
		httpclient.WithRetryMax(1), fastRetries)
	err := doGet(t, c, t.Context(), secretURL("http://example.org"))
	require.Error(t, err)
	require.Contains(t, err.Error(), querySecret, "net/http quotes the full URL")

	redacted := httpclient.RedactError(err)
	requireRedacted(t, redacted, "")
	require.ErrorIs(t, redacted, syscall.ECONNRESET)
	require.ErrorIs(t, redacted, httpclient.ErrMaxRetriesExceeded)
	var urlErr *url.Error
	require.ErrorAs(t, redacted, &urlErr)
	require.Equal(t, "http://example.org/orders", urlErr.URL)
	before := redacted.Error()
	require.Equal(t, before, httpclient.RedactError(redacted).Error())

	require.NoError(t, httpclient.RedactError(nil))
	plain := errors.New("no url " + querySecret)
	require.Same(t, plain, httpclient.RedactError(plain), "an error without a *url.Error is returned unchanged")
}

// net/http quotes an unparsable redirect Location, server-supplied and with
// its query, in the nested text of the *url.Error: RedactError replaces it.
func TestRedactErrorLocationHeader(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://bad host/next?token="+querySecret)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	c := httpclient.New(loopback, httpclient.WithoutRetry())
	err := doGet(t, c, t.Context(), srv.URL+"/start")
	require.Error(t, err)
	require.Contains(t, err.Error(), querySecret, "net/http quotes the Location header")

	err = httpclient.RedactError(err)
	requireRedacted(t, err, "")
	var urlErr *url.Error
	require.ErrorAs(t, err, &urlErr)
	require.Equal(t, srv.URL+"/start", urlErr.URL)
}

// refusedAddr returns a loopback address nothing listens on.
func refusedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

// The core/errors classifiers callers apply to a *url.Error keep working
// after RedactError, on the plain *http.Client and through the HTTPClient
// wrappers and the request builder, which apply it themselves.
func TestRedactErrorKeepsClassifiers(t *testing.T) {
	t.Parallel()

	redirectLoop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop?sig="+querySecret, http.StatusFound)
	}))
	t.Cleanup(redirectLoop.Close)
	untrusted := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(untrusted.Close)
	stalled := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(stalled.Close)

	scenarios := []struct {
		name     string
		target   string
		classify func(error) bool
		timeout  bool
	}{
		{name: "redirects", target: redirectLoop.URL + "/start?token=" + querySecret, classify: network.IsResourceRedirects},
		{name: "unsupported scheme", target: "ftp://example.org/orders?token=" + querySecret, classify: network.IsUnsupportedProtocolScheme},
		{name: "unknown authority", target: secretURL(untrusted.URL), classify: network.IsCertUnknownAuthority},
		{name: "connection refused", target: secretURL("http://" + refusedAddr(t)), classify: network.IsConnectionRefused},
		{name: "timeout", target: secretURL(stalled.URL), classify: network.IsRequestTimeoutError, timeout: true},
	}
	callers := []struct {
		name string
		call func(ctx context.Context, target string) error
	}{
		{name: "plain client", call: func(ctx context.Context, target string) error {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				return err
			}
			resp, err := httpclient.New(loopback, httpclient.WithRetryMax(1), fastRetries).Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			return httpclient.RedactError(err)
		}},
		{name: "wrapper", call: func(ctx context.Context, target string) error {
			resp, err := httpclient.NewHTTPClient(loopback, httpclient.WithRetryMax(1), fastRetries).Get(ctx, target)
			if resp != nil {
				_ = resp.Body.Close()
			}
			return err
		}},
		{name: "builder", call: func(ctx context.Context, target string) error {
			c := httpclient.NewHTTPClient(loopback, httpclient.WithRetryMax(1), fastRetries)
			resp, err := httpclient.NewRequestBuilder(c).GET(target).Send(ctx)
			if resp != nil {
				_ = resp.Body.Close()
			}
			return err
		}},
	}
	for _, sc := range scenarios {
		for _, caller := range callers {
			t.Run(sc.name+"/"+caller.name, func(t *testing.T) {
				t.Parallel()

				ctx := t.Context()
				if sc.timeout {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
					defer cancel()
				}
				err := caller.call(ctx, sc.target)
				require.Error(t, err)
				requireRedacted(t, err, "")
				require.True(t, sc.classify(err), "%v", err)
				require.True(t, network.IsURLError(err))
				require.True(t, network.IsNetworkError(err))
			})
		}
	}
}

// errHandled is an ErrorHandler's own sentinel, which must stay classifiable.
var errHandled = errors.New("handled")

// An error an ErrorHandler returns may quote the request URL: it is redacted
// before the retry layer returns it, and keeps its causes.
func TestRedactionErrorHandlerError(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusBadRequest, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
			t.Cleanup(srv.Close)
			handler := func(resp *http.Response, err error, _ int) (*http.Response, error) {
				require.NotNil(t, resp)
				return resp, fmt.Errorf("handler for %s: %w: %w", resp.Request.URL, errHandled, err)
			}
			c := httpclient.New(loopback, httpclient.WithRetryMax(1), fastRetries, httpclient.WithErrorHandler(handler))
			err := roundTripError(t, doGet(t, c, t.Context(), secretURL(srv.URL)))
			requireRedacted(t, err, "")
			require.ErrorIs(t, err, errHandled)
			se := httpclient.IsUnexpectedStatusError(err)
			require.NotNil(t, se, "%v", err)
			require.Equal(t, status, se.Status)
			if status == http.StatusServiceUnavailable {
				require.ErrorIs(t, err, httpclient.ErrMaxRetriesExceeded)
			}

			redacted := httpclient.RedactError(doGet(t, c, t.Context(), secretURL(srv.URL)))
			requireRedacted(t, redacted, "")
			require.ErrorIs(t, redacted, errHandled)
		})
	}
}

// A typed status error returned by a policy or an error handler is rendered
// from its status and request fields only: its Body is upstream text that may
// echo the request URL, and a caller-set URI or Host may carry a query.
func TestRedactionTypedStatusError(t *testing.T) {
	t.Parallel()

	typed := func(req *http.Request) error {
		return &httpclient.UnexpectedStatusError{
			Status: http.StatusBadRequest,
			Host:   "user:" + passwordSecret + "@" + req.URL.Host,
			URI:    req.URL.RequestURI(),
			Method: req.Method,
			Body:   []byte("upstream rejected " + req.URL.String()),
		}
	}
	tests := []struct {
		name string
		opt  httpclient.Option
	}{
		{"retry policy", httpclient.WithRetryPolicyHandler(func(_ context.Context, resp *http.Response, err error) (bool, error) {
			if err != nil {
				return false, err
			}
			return false, typed(resp.Request)
		})},
		{"error handler", httpclient.WithErrorHandler(func(resp *http.Response, _ error, _ int) (*http.Response, error) {
			return resp, typed(resp.Request)
		})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
			}))
			t.Cleanup(srv.Close)
			var logs syncBuffer
			c := httpclient.New(loopback, httpclient.WithRetryMax(1), fastRetries, debugLogger(&logs), tc.opt)
			err := roundTripError(t, doGet(t, c, t.Context(), secretURL(srv.URL)))
			requireRedacted(t, err, logs.String())
			se := httpclient.IsUnexpectedStatusError(err)
			require.NotNil(t, se, "%v", err)
			require.Equal(t, http.StatusBadRequest, se.Status)
			require.Contains(t, err.Error(), "unexpected response status: 400")
		})
	}
}

// Host-like fields of the typed errors a policy may return are reduced to the
// host itself, so a whole URL put into them cannot carry its query along.
func TestRedactionTypedIdentityFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		typed func(u string) error
		check func(t *testing.T, err error)
	}{
		{"status error", func(u string) error {
			return &httpclient.UnexpectedStatusError{Status: http.StatusBadRequest, Host: u, URI: "/x?token=" + querySecret, Method: "GET"}
		}, func(t *testing.T, err error) { require.NotNil(t, httpclient.IsUnexpectedStatusError(err)) }},
		{"ssrf error", func(u string) error {
			return &httpclient.SSRFError{Host: u, IP: u, Rule: "deny[0] " + u}
		}, func(t *testing.T, err error) { require.ErrorIs(t, err, httpclient.ErrSSRFBlocked) }},
		{"rate limit error", func(u string) error {
			return &limiters.RateLimitError{Host: u}
		}, func(t *testing.T, err error) { require.ErrorIs(t, err, limiters.ErrRateLimited) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) }))
			t.Cleanup(srv.Close)
			var logs syncBuffer
			c := httpclient.New(loopback, httpclient.WithRetryMax(1), fastRetries, debugLogger(&logs),
				httpclient.WithRetryPolicyHandler(func(_ context.Context, resp *http.Response, err error) (bool, error) {
					if err != nil {
						return false, err
					}
					return false, tc.typed(resp.Request.URL.String())
				}))
			err := roundTripError(t, doGet(t, c, t.Context(), secretURL(srv.URL)))
			requireRedacted(t, err, logs.String())
			tc.check(t, err)
		})
	}
}

// HTTPClient.Do redacts like the other HTTPClient methods.
func TestHTTPClientDoRedacts(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	c := httpclient.NewHTTPClient(httpclient.WithoutRetry(), httpclient.WithoutSSRFProtection())
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://user:"+passwordSecret+"@"+addr+"/path?token="+querySecret, nil)
	require.NoError(t, err)
	resp, err := c.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	require.NotContains(t, err.Error(), querySecret)
	require.NotContains(t, err.Error(), passwordSecret)
	_, isURLErr := errors.AsType[*url.Error](err)
	require.True(t, isURLErr)
	require.True(t, network.IsConnectionRefused(err), "%v", err)
}

// A rule label built by an EgressPolicy survives redaction; another shape does not.
func TestRedactionKeepsEgressRuleLabels(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		rule, want string
	}{
		{rule: "deny[0] host=*.internal port=22", want: "deny[0] host=*.internal port=22"},
		{rule: "deny[3] cidr=10.0.0.0/8", want: "deny[3] cidr=10.0.0.0/8"},
		{rule: "deny[1] host=api?.example", want: "deny[1] host=api?.example"},
		{rule: "deny[0] http://x/?token=" + querySecret, want: "(unlabeled rule)"},
		{rule: "deny[0] host=x?token=" + querySecret, want: "(unlabeled rule)"},
		{rule: "blockMetadata cidr=169.254.0.0/16", want: "blockMetadata cidr=169.254.0.0/16"},
		{rule: "allowedCIDRs[1] cidr=10.0.0.0/8", want: "allowedCIDRs[1] cidr=10.0.0.0/8"},
		{rule: "block metadata?token=" + querySecret, want: "(unlabeled rule)"},
		{rule: strings.Repeat("a", 65) + " port=22", want: "(unlabeled rule)"},
	} {
		err := httpclient.RedactError(&url.Error{Op: "Get", URL: "http://h/", Err: &httpclient.SSRFError{Host: "h", Rule: tc.rule}})
		require.Contains(t, err.Error(), tc.want)
		require.NotContains(t, err.Error(), querySecret)
	}
}

// An SSRF refusal is never retried, even when a custom policy asks for it.
func TestSSRFRefusalIgnoresRetryingPolicy(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		cause error
	}{
		{"typed", &httpclient.SSRFError{Host: "internal.test", IP: "10.0.0.1"}},
		{"sentinel", fmt.Errorf("dial policy: %w", httpclient.ErrSSRFBlocked)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var calls atomic.Int32
			refusing := httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return nil, tc.cause
			})
			c := httpclient.New(httpclient.WithoutSSRFProtection(), httpclient.WithClient(&http.Client{Transport: refusing}),
				httpclient.WithRetryMax(3), fastRetries,
				httpclient.WithRetryPolicyHandler(func(context.Context, *http.Response, error) (bool, error) { return true, nil }))
			err := roundTripError(t, doGet(t, c, t.Context(), "http://example.org/"))
			require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)
			require.ErrorIs(t, err, httpclient.ErrNonRetryable)
			require.Equal(t, int32(1), calls.Load())
		})
	}
}

// A retry-exhausted error built by a caller renders its method only as a
// method token and its URL without the query.
func TestRedactErrorExhaustedFields(t *testing.T) {
	t.Parallel()

	cause := &httpclient.RetryExhaustedError{Attempts: 2, Method: leakURL, URL: leakURL, LastError: errors.New("?token=" + querySecret)}
	err := httpclient.RedactError(&url.Error{Op: "Get", URL: leakURL, Err: cause})
	requireRedacted(t, err, "")
	require.ErrorIs(t, err, cause)
	require.Contains(t, err.Error(), "failed after 2 attempts for ? http://example.org/orders: HTTP transport error (*errors.errorString)")
}
