// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/httpx"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

// secretWrapper is a caller's error type that renders the error it wraps into its own text.
type secretWrapper struct{ err error }

func (w *secretWrapper) Error() string { return "custom: " + w.err.Error() }
func (w *secretWrapper) Unwrap() error { return w.err }

// TestRedactErrorWrapped: review finding R22-1 — RedactError redacted the first *url.Error in place but returned its wrapper as it
// was, and a wrapper such as fmt.Errorf("fetch: %w", urlErr) had already captured the URL with its query string in its own text
// (verified on c87267e6: the result still read token=secret). Every *url.Error of the tree is redacted, and a wrapper is returned
// inside an error whose text is the redacted *url.Errors' only; errors.Is and errors.As still reach the causes.
func TestRedactErrorWrapped(t *testing.T) {
	t.Parallel()

	urlErr := func(rawURL string) *url.Error { return &url.Error{Op: "Get", URL: rawURL, Err: io.EOF} }
	const secret = "token=secret"
	tests := []struct {
		name string
		err  func() error
	}{
		{"fmt wrapper", func() error { return fmt.Errorf("fetch: %w", urlErr("https://example.org/?"+secret)) }},
		{"two fmt wrappers", func() error {
			return fmt.Errorf("outer: %w", fmt.Errorf("fetch: %w", urlErr("https://u:p@example.org/a?"+secret+"#f")))
		}},
		{"joined", func() error { return errors.Join(errors.New("first"), urlErr("https://example.org/?"+secret)) }},
		{"joined twice", func() error {
			return errors.Join(urlErr("https://a.example/?"+secret), urlErr("https://b.example/?"+secret))
		}},
		{"multi-%w", func() error {
			return fmt.Errorf("both: %w and %w", io.ErrUnexpectedEOF, urlErr("https://example.org/?"+secret))
		}},
		{"custom wrapper", func() error { return &secretWrapper{err: urlErr("https://example.org/?" + secret)} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := httpclient.RedactError(tt.err())
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret")
			require.NotContains(t, err.Error(), "u:p@")
			require.Contains(t, err.Error(), "example")
			require.ErrorIs(t, err, io.EOF)
			ue, ok := errors.AsType[*url.Error](err)
			require.True(t, ok)
			require.NotContains(t, ue.URL, "secret", "errors.As finds the redacted copy")

			again := httpclient.RedactError(err)
			require.Same(t, err, again, "redacting again is a no-op")
		})
	}

	direct := urlErr("https://example.org/?" + secret)
	redacted := httpclient.RedactError(direct)
	require.NotContains(t, redacted.Error(), "secret")
	require.Equal(t, `Get "https://example.org/": EOF`, redacted.Error(), "the operation is kept")
	require.ErrorIs(t, redacted, io.EOF)
	require.Contains(t, direct.Error(), "secret", "the caller's error is not modified")
	require.Same(t, redacted, httpclient.RedactError(redacted), "a redacted *url.Error is returned unchanged")
	plain := fmt.Errorf("no URL: %w", io.EOF)
	require.Same(t, plain, httpclient.RedactError(plain), "an error without a *url.Error is returned unchanged")
}

// TestRedactErrorBeyondTraversalLimit: review finding R23-1 — the traversal of the error tree stopped after 1,024 errors without
// telling, so a *url.Error joined after 1,024 other errors was never seen and the error was returned with its secret (verified on
// dee58922). An error whose tree cannot be inspected fully is replaced by an error of a fixed text wrapping ErrUninspectableCause.
func TestRedactErrorBeyondTraversalLimit(t *testing.T) {
	t.Parallel()

	errs := make([]error, 0, 1025)
	for range 1024 {
		errs = append(errs, io.ErrUnexpectedEOF)
	}
	errs = append(errs, &url.Error{Op: "Get", URL: "https://example.org/?token=secret", Err: io.EOF})
	wide := errors.Join(errs...)

	deep := error(&url.Error{Op: "Get", URL: "https://example.org/?token=secret", Err: io.EOF})
	for range 1100 {
		deep = fmt.Errorf("layer: %w", deep)
	}
	for name, err := range map[string]error{"wide": wide, "deep": deep} {
		redacted := httpclient.RedactError(err)
		require.NotContains(t, redacted.Error(), "secret", name)
		// Review finding R25-2: the causes of a graph too large to inspect are dropped, not kept for errors.Is to search.
		require.ErrorIs(t, redacted, httpclient.ErrUninspectableCause, name)
		require.NotErrorIs(t, redacted, io.EOF, name)
		require.Same(t, redacted, httpclient.RedactError(redacted), name)
	}
}

// TestRedactErrorSharedCause: review finding R23-2 — RedactError rewrote every *url.Error of the tree in place, so a transport
// returning one shared *url.Error to concurrent requests had its fields written by several goroutines at once while others read
// them (a data race under -race, verified on dee58922). RedactError modifies nothing: the shared error keeps its text.
func TestRedactErrorSharedCause(t *testing.T) {
	t.Parallel()

	shared := &url.Error{Op: "Get", URL: "https://upstream.example/?token=secret", Err: io.ErrUnexpectedEOF}
	transport := httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, shared })
	c := httpclient.NewHTTPClient(httpclient.WithoutSSRFProtection(), httpclient.WithClient(&http.Client{Transport: transport}),
		httpclient.WithoutRetry())

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				resp, err := c.Get(t.Context(), "https://example.org/?token=secret")
				if resp != nil {
					httpclient.CloseBody(resp.Body)
				}
				if err == nil || strings.Contains(err.Error(), "secret") {
					t.Errorf("err = %v, want a redacted error", err)
					return
				}
			}
		})
	}
	wg.Wait()
	require.Equal(t, "https://upstream.example/?token=secret", shared.URL, "the transport's error is not modified")
}

// selfCause is an error whose cause is itself.
type selfCause struct{}

func (e *selfCause) Error() string { return "self" }
func (e *selfCause) Unwrap() error { return e }

// TestRedactErrorExplosiveCauses: review findings R24-1, R25-1 and R25-2 — the cause of a *url.Error was rendered by errors.As and
// errors.Is searches outside the traversal bound: a small acyclic graph of repeated causes (errors.Join(e, e) nested forty times)
// took exponentially many visits and a cause unwrapping to itself never returned (verified on dbb8bfcf); the circuit breaker
// formatted the transport's error (its own exponential Error()) before the retry layer bounded it, and RedactError kept a cause
// graph it could not inspect, which errors.Is on its result, and the retry layer behind a policy returning it, searched without
// end (verified on 1d3a536a). Every error the client or RedactError returns holds a bounded cause graph or ErrUninspectableCause;
// each call below returns promptly with the fixed text, and classifying its result returns too.
func TestRedactErrorExplosiveCauses(t *testing.T) {
	t.Parallel()

	doubled := errors.New("failure")
	for range 40 {
		doubled = errors.Join(doubled, doubled)
	}
	self := error(&selfCause{})
	clientGet := func(cause error, opts ...httpclient.Option) func() error {
		return func() error {
			transport := httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, cause })
			c := httpclient.NewHTTPClient(append([]httpclient.Option{
				httpclient.WithoutSSRFProtection(), httpclient.WithClient(&http.Client{Transport: transport}),
			}, opts...)...)
			resp, err := c.Get(t.Context(), "https://example.org/?token=secret")
			if resp != nil {
				httpclient.CloseBody(resp.Body)
			}
			return err
		}
	}
	retryOnce := []httpclient.Option{httpclient.WithRetryMax(1), httpclient.WithRetryWait(time.Millisecond, time.Millisecond)}
	policy := httpclient.WithRetryPolicyHandler(func(context.Context, *http.Response, error) (bool, error) {
		return false, httpclient.RedactError(self)
	})
	calls := map[string]func() error{
		"direct doubled": func() error {
			return httpclient.RedactError(&url.Error{Op: "Get", URL: "https://example.org/?token=secret", Err: doubled})
		},
		"direct self": func() error {
			return httpclient.RedactError(&url.Error{Op: "Get", URL: "https://x/?token=secret", Err: self})
		},
		"direct self wrapped": func() error { return httpclient.RedactError(fmt.Errorf("token=secret: %w", self)) },
		"client self":         clientGet(self, retryOnce...),
		"client doubled":      clientGet(doubled, retryOnce...),
		"client doubled once": clientGet(doubled, httpclient.WithoutRetry()),
		"client self policy":  clientGet(io.EOF, append(retryOnce, policy)...),
		"client doubled policy": clientGet(io.EOF, append(retryOnce, httpclient.WithRetryPolicyHandler(
			func(context.Context, *http.Response, error) (bool, error) { return false, doubled }))...),
	}
	for name, call := range calls {
		done := make(chan error, 1)
		go func() {
			err := call()
			_ = errors.Is(err, context.Canceled) // classification must return too
			_, _ = errors.AsType[*url.Error](err)
			done <- err
		}()
		select {
		case err := <-done:
			require.Error(t, err, name)
			require.NotContains(t, err.Error(), "secret", name)
			require.Contains(t, err.Error(), "HTTP transport error (redacted)", name)
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: did not return", name)
		}
	}
}
