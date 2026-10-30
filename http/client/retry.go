// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package client

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/altessa-s/go-atlas-core/errors/network"
	"github.com/altessa-s/go-atlas-core/time/clock"

	"github.com/altessa-s/go-atlas-net/http/client/limiters"

	coreerrs "github.com/altessa-s/go-atlas-core/errors"
	coreio "github.com/altessa-s/go-atlas-core/io"
	coreretry "github.com/altessa-s/go-atlas-core/retry"
)

// statusToClass converts an HTTP status code to a class string (2xx, 3xx, 4xx, 5xx).
func statusToClass(code int) string {
	switch {
	case code >= http.StatusOK && code < http.StatusMultipleChoices:
		return "2xx"
	case code >= http.StatusMultipleChoices && code < http.StatusBadRequest:
		return "3xx"
	case code >= http.StatusBadRequest && code < http.StatusInternalServerError:
		return "4xx"
	case code >= http.StatusInternalServerError:
		return "5xx"
	default:
		return "unknown"
	}
}

// retryableStatusError is an internal sentinel indicating a retryable HTTP
// status. retryAfter is the pause the server asked for (429/503 Retry-After,
// capped), zero when absent.
type retryableStatusError struct {
	status     int
	retryAfter time.Duration
}

func (e *retryableStatusError) Error() string {
	return fmt.Sprintf("retryable HTTP status: %d", e.status)
}

// retryRoundTripper wraps an [http.RoundTripper] with retry logic powered by
// [coreretry.Do]. It replaces the former retryablehttp.Client approach with a
// composable RoundTripper that sits in the standard transport chain.
type retryRoundTripper struct {
	next               http.RoundTripper
	retryOpts          []coreretry.Option // base retry options (cap, delay policy)
	maxAttempts        int                // mirrors WithMaxAttempts for metrics/error reporting
	logger             *slog.Logger
	errorHandler       ErrorHandler
	retryPolicyHandler RetryPolicyHandler
	observer           Observer
	// attemptTimeout bounds every attempt (limiter wait under
	// WithLimiterPerAttempt, connection, reply and body); non-positive leaves
	// attempts bounded by the request context only.
	attemptTimeout time.Duration
	// jar, when set, is owned by the retry layer (http.Client.Jar is nil):
	// every attempt sends the caller's Cookie header plus the jar's cookies
	// for the URL, and every reply's cookies are stored once, on receipt.
	jar http.CookieJar
	// retryAfterMax caps an honored Retry-After; non-positive ignores it.
	retryAfterMax time.Duration
	// retryAfterStatuses are the statuses whose Retry-After is honored.
	retryAfterStatuses []int
	// retryableStatuses are the reply statuses the built-in classification
	// retries (see WithRetryableStatuses).
	retryableStatuses []int
	// clock is the time source of the round tripper: the Retry-After of an
	// HTTP date is relative to its Now, and the request duration reported to
	// the observer is measured with it. Nil means the system clock.
	clock clock.Clock
}

// now returns the current time of the round tripper's clock.
func (rt *retryRoundTripper) now() time.Time {
	if rt.clock == nil {
		return clock.Real().Now()
	}
	return rt.clock.Now()
}

// retryableStatus builds the retryable error of resp, carrying its
// Retry-After when the round tripper honors it for that status.
func (rt *retryRoundTripper) retryableStatus(resp *http.Response) *retryableStatusError {
	e := &retryableStatusError{status: resp.StatusCode}
	if rt.retryAfterMax > 0 && honorsRetryAfter(rt.retryAfterStatuses, resp.StatusCode) {
		e.retryAfter, _ = ParseRetryAfter(resp.Header, rt.now(), rt.retryAfterMax)
	}
	return e
}

// retryAfterDelay wraps backoff so a retry after a 429/503 that carried
// Retry-After waits at least as long as the server asked.
func retryAfterDelay(backoff coreretry.NextDelayFunc) coreretry.NextDelayFunc {
	return func(attempt int, err error) time.Duration {
		d := backoff(attempt, err)
		if rs, ok := errors.AsType[*retryableStatusError](err); ok && d > 0 {
			d = max(d, rs.retryAfter)
		}
		return d
	}
}

// attemptRequest returns the request of one attempt: with a cookie jar, a
// copy whose Cookie header is the caller's original one (callerCookie,
// captured before any attempt) plus the jar's cookies for the URL read now;
// with an attempt timeout, a copy under its own deadline, whose cancel func
// is returned.
func (rt *retryRoundTripper) attemptRequest(
	ctx context.Context, req *http.Request, callerCookie []string,
) (*http.Request, context.CancelFunc) {
	out := req
	if rt.jar != nil {
		out = req.Clone(ctx)
		out.Header.Del("Cookie")
		for _, v := range callerCookie {
			out.Header.Add("Cookie", v)
		}
		for _, c := range rt.jar.Cookies(cookieURL(req)) {
			out.AddCookie(c)
		}
	}
	if rt.attemptTimeout <= 0 {
		return out, nil
	}
	actx, cancel := context.WithTimeout(ctx, rt.attemptTimeout)
	return out.WithContext(actx), cancel
}

// cookieURL is the URL cookies of req are looked up and stored under, as in
// net/http's Client.send: req.URL with req.Host as the authority when set.
func cookieURL(req *http.Request) *url.URL {
	if req.Host == "" {
		return req.URL
	}
	u := req.URL.Clone()
	u.Host = req.Host
	return u
}

// filterRedirectCookies mirrors net/http's makeHeadersCopier for a jar. It
// runs from the client's CheckRedirect (see redirectPolicy), after
// http.Client copied the initial request's headers into req and before the
// caller's redirect policy: http.Client's own Jar is nil, so it forwarded the
// initial Cookie header unchanged; with a jar it would drop every initial
// cookie whose name a redirect reply of the chain set (replaced or deleted,
// regardless of domain and path) and rebuild the header sorted. The replies
// are req.Response and the Response of every later request in via.
func filterRedirectCookies(req *http.Request, via []*http.Request) {
	if len(via) == 0 || via[0].Header.Get("Cookie") == "" || len(req.Header.Values("Cookie")) == 0 {
		return // no initial cookies, or http.Client stripped them (other domain)
	}
	set := make(map[string]struct{})
	addCookieNames(set, req.Response)
	for _, r := range via[1:] {
		addCookieNames(set, r.Response)
	}
	if len(set) == 0 {
		return
	}
	var kept []string
	changed := false
	for _, c := range via[0].Cookies() {
		if _, ok := set[c.Name]; ok {
			changed = true
			continue
		}
		kept = append(kept, c.Name+"="+c.Value)
	}
	if !changed {
		return
	}
	slices.Sort(kept)                                  // as net/http, for a deterministic header
	req.Header.Set("Cookie", strings.Join(kept, "; ")) // #nosec G119 -- a subset of the Cookie header net/http already forwarded
}

// addCookieNames adds the names of the cookies resp sets to set.
func addCookieNames(set map[string]struct{}, resp *http.Response) {
	if resp == nil {
		return
	}
	for _, c := range resp.Cookies() {
		set[c.Name] = struct{}{}
	}
}

// maxRedirects is net/http's default redirect limit.
const maxRedirects = 10

// redirectPolicy wraps the client's CheckRedirect for a jar owned by the
// retry layer: the initial cookies are filtered first (filterRedirectCookies),
// then the caller's policy runs — or net/http's default, which stops after 10
// redirects — so a cookie the policy sets on req is kept.
func redirectPolicy(check func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		filterRedirectCookies(req, via)
		if check != nil {
			return check(req, via)
		}
		if len(via) >= maxRedirects {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
}

// storeCookies saves the cookies resp set for req's URL in the jar. It runs
// once per reply, when the reply is received.
func (rt *retryRoundTripper) storeCookies(req *http.Request, resp *http.Response) {
	if rt.jar == nil || resp == nil {
		return
	}
	if rc := resp.Cookies(); len(rc) > 0 {
		rt.jar.SetCookies(cookieURL(req), rc)
	}
}

// releaseWithBody ties an attempt's cancel func to its reply body, so the
// attempt deadline covers reading the body; on failure it cancels at once.
func releaseWithBody(resp *http.Response, err error, cancel context.CancelFunc) *http.Response {
	if cancel == nil {
		return resp
	}
	if err != nil || resp == nil || resp.Body == nil {
		cancel()
		return resp
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp
}

// cancelOnClose releases an attempt's deadline when its body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

// Close closes the body and releases the attempt deadline.
func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// CloseIdleConnections forwards to the wrapped transport so
// [http.Client.CloseIdleConnections] releases pooled connections.
func (rt *retryRoundTripper) CloseIdleConnections() { closeIdleConnections(rt.next) }

// RoundTrip implements [http.RoundTripper]. On the first call it buffers the
// request body (if any) so it can be replayed on retries, then delegates retry
// orchestration to [coreretry.Do]. Every call reports exactly one RequestDone
// to the observer, timed from the start of the call (buffering included).
func (rt *retryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	start := rt.now()
	replay := rt.replayable(req)
	if replay {
		if err := rt.bufferBody(req); err != nil {
			rt.requestDone(req, start, "unknown", 0, true)
			return nil, err
		}
	}

	ctx := req.Context()

	call := &retryCall{
		rt:  rt,
		req: req,
		ctx: ctx,
		// The caller's Cookie header as received: with a jar the retry layer
		// owns cookie handling (http.Client.Jar is nil), so nothing was injected;
		// on a redirect hop it was already filtered by redirectPolicy.
		callerCookie: req.Header.Values("Cookie"),
		firstAttempt: true,
	}
	perRequestOpts := append(make([]coreretry.Option, 0, len(rt.retryOpts)+2), rt.retryOpts...)
	perRequestOpts = append(perRequestOpts,
		coreretry.WithShouldRetry(retryable),
		coreretry.WithOnRetry(call.onRetry),
	)
	if !replay {
		perRequestOpts = append(perRequestOpts, coreretry.WithMaxAttempts(0))
	}

	retryErr := coreretry.Do(ctx, call.try, perRequestOpts...)
	if !call.bodyHanded && req.Body != nil {
		// No attempt reached the next round tripper (the context ended before
		// the first one): the request body — the caller's or the replay copy
		// installed by bufferBody — is still ours to close, as
		// http.RoundTripper requires.
		_ = req.Body.Close()
	}

	statusClass := "unknown"
	if call.lastResp != nil {
		statusClass = statusToClass(call.lastResp.StatusCode)
	}

	if retryErr != nil && ctx.Err() == nil && retryable(retryErr) {
		retryErr = exhausted(req, call.lastResp, retryErr, call.attempts)
	}
	// A pass-through policy (WithoutRetry) clears retryErr but leaves the
	// attempt's error in lastErr, which finish returns: still a failure.
	failed := retryErr != nil || call.lastErr != nil
	resp, err := rt.finish(call.lastResp, call.lastErr, retryErr, call.attempts)
	rt.requestDone(req, start, statusClass, call.retryAttempts, failed)
	return resp, err
}

// requestDone reports the completion of the round trip of req, started at
// start, to the observer.
func (rt *retryRoundTripper) requestDone(req *http.Request, start time.Time, statusClass string, retries int, failed bool) {
	rt.observer.RequestDone(RequestEvent{
		Method:      req.Method,
		Host:        req.URL.Hostname(),
		StatusClass: statusClass,
		Retries:     retries,
		Failed:      failed,
		Duration:    rt.now().Sub(start),
	})
}

// replayable reports whether the retry layer may send req more than once.
// With a [RetryPolicyHandler] the policy decides. The built-in classification
// replays only requests that are safe to repeat, as [net/http.Transport] does
// for its own retries: an idempotent method (GET, HEAD, OPTIONS, TRACE, PUT,
// DELETE) or a request carrying an Idempotency-Key or X-Idempotency-Key header.
// Any other request (POST, PATCH, ...) is attempted once, so a server that
// processed it before failing never sees it twice.
func (rt *retryRoundTripper) replayable(req *http.Request) bool {
	if rt.retryPolicyHandler != nil {
		return true
	}
	switch req.Method {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodPut, http.MethodDelete:
		return true
	}
	return req.Header.Get("Idempotency-Key") != "" || req.Header.Get("X-Idempotency-Key") != ""
}

// bufferBody prepares req's body for replay. It prefers the caller-provided
// GetBody (net/http sets it automatically for bytes/strings readers) and
// buffers only when retries are possible (maxAttempts counts retries;
// negative means unlimited) and no replay source exists.
//
// Buffering honors the request's context: an ended context closes the body
// and fails with the context's error without reading; one ending while the
// read is blocked (a pipe whose writer stays open) closes the body, which
// unblocks the read, and fails the same way. The body is closed exactly once.
func (rt *retryRoundTripper) bufferBody(req *http.Request) error {
	if req.Body == nil || req.GetBody != nil || rt.maxAttempts == 0 {
		return nil
	}
	ctx := req.Context()
	closeBody := sync.OnceValue(req.Body.Close)
	if err := ctx.Err(); err != nil {
		_ = closeBody()
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = closeBody() })
	buf := coreio.GetBuffer()
	_, err := buf.ReadFrom(req.Body)
	stop()
	_ = closeBody()
	if err != nil {
		coreio.PutBuffer(buf)
		if ctxErr := ctx.Err(); ctxErr != nil {
			// The read failed because the context ended and closed the body.
			return ctxErr
		}
		// The body's error text is the caller's: render a trusted one.
		return coreerrs.Wrapf(redactTransportError(err), "failed to read request body for retry buffering")
	}
	bodyBytes := bytes.Clone(buf.Bytes())
	coreio.PutBuffer(buf)

	req.ContentLength = int64(len(bodyBytes))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(bodyBytes)), nil
	}
	// The original body was consumed by buffering; install the replay
	// copy for the first attempt.
	req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	return nil
}

// retryCall is the state of one RoundTrip across its attempts.
type retryCall struct {
	rt  *retryRoundTripper
	req *http.Request
	// ctx is the request's context, used for retry logging.
	ctx          context.Context
	callerCookie []string
	lastResp     *http.Response
	lastErr      error
	// retryAttempts counts the retries made so far.
	retryAttempts int
	// attempts counts the attempts handed to the next round tripper so far.
	attempts     int
	firstAttempt bool
	// bodyHanded reports whether an attempt handed the request body to the
	// next round tripper, which then owns closing it.
	bodyHanded bool
}

// onRetry runs before each retry's backoff sleep.
func (c *retryCall) onRetry(attempt int, err error, delay time.Duration) {
	c.retryAttempts++
	// A retry follows: release the discarded response before the
	// backoff sleep so its connection returns to the pool meanwhile.
	// With an ErrorHandler the response must stay readable in case
	// the context ends during the sleep and the handler receives it.
	if c.rt.errorHandler == nil && c.lastResp != nil && c.lastResp.Body != nil {
		closeBody(c.lastResp)
		c.lastResp.Body = http.NoBody
	}
	c.rt.observer.Retry(AttemptEvent{Method: c.req.Method, Host: c.req.URL.Hostname()})
	if c.rt.logger != nil {
		c.rt.logger.DebugContext(c.ctx,
			"http client retrying request",
			slog.Int("attempt", attempt),
			slog.String("method", c.req.Method),
			// Omit user info and the query string: outbound URLs may
			// carry signed-URL credentials or tokens that must not reach
			// debug logs. err renders trusted text only (transportError).
			slog.String("url", redactedURL(c.req.URL)),
			slog.String("error", err.Error()),
			slog.Duration("delay", delay),
		)
	}
}

// try makes one attempt of the round trip and classifies its outcome for
// coreretry.Do.
func (c *retryCall) try(ctx context.Context) error {
	// Close previous response body if present from a prior attempt.
	closeBody(c.lastResp)
	c.lastResp = nil

	// Rewind the body for retry attempts; the first attempt uses the
	// body installed by bufferBody (or the caller's original one).
	if !c.firstAttempt && c.req.GetBody != nil {
		body, gbErr := c.req.GetBody()
		if gbErr != nil {
			c.lastErr = coreerrs.Wrapf(redactTransportError(gbErr), "failed to rewind request body for retry")
			return &NonRetryableError{Err: c.lastErr}
		}
		c.req.Body = body
	}
	c.firstAttempt = false

	attemptReq, cancel := c.rt.attemptRequest(ctx, c.req, c.callerCookie)
	c.attempts++
	c.bodyHanded = true
	resp, err := c.rt.next.RoundTrip(attemptReq) //nolint:bodyclose // resp is stored in lastResp and either closed on next retry or returned to caller
	// From here on a transport error renders trusted text only: its own
	// text may embed the request URL with its query string.
	err = redactTransportError(err)
	resp = releaseWithBody(resp, err, cancel) //nolint:bodyclose // same response, closed like resp above
	// Every reply's cookies are stored exactly once, on receipt, whatever
	// happens to the reply next (retried, returned, discarded).
	c.rt.storeCookies(c.req, resp)
	c.lastResp = resp
	c.lastErr = err
	// A breaker rejection is reported from the attempt's own outcome, once,
	// before any classification: a retry policy (WithoutRetry's pass-through
	// among them) may replace or suppress the error the caller then gets.
	if IsCircuitBreakerOpen(err) {
		c.rt.observer.BreakerRejected(AttemptEvent{Method: c.req.Method, Host: c.req.URL.Hostname()})
	}

	// If user supplied a custom RetryPolicyHandler, delegate to it.
	if c.rt.retryPolicyHandler != nil {
		return c.applyPolicy(ctx, resp, err)
	}

	// Default classification.
	if err != nil {
		return classifyTransportError(ctx, err)
	}

	classified := c.rt.classifyResponse(resp)
	c.lastErr = classified
	return classified
}

// applyPolicy classifies an attempt's outcome with the caller's
// RetryPolicyHandler.
func (c *retryCall) applyPolicy(ctx context.Context, resp *http.Response, err error) error {
	retry, policyErr := c.rt.retryPolicyHandler(ctx, resp, err)
	// The handler's error is logged, stored and returned: it renders
	// trusted text only, as a transport error does (it may quote
	// resp.Request.URL, for example).
	policyErr = redactTransportError(policyErr)
	// An SSRF refusal is final: a policy asking to retry it is overruled.
	if refusal := cmp.Or(ssrfRefusal(policyErr), ssrfRefusal(err)); retry && refusal != nil {
		c.lastErr = refusal
		return &NonRetryableError{Err: refusal}
	}
	if policyErr != nil {
		c.lastErr = policyErr
		if !retry {
			return &NonRetryableError{Err: policyErr}
		}
		return policyErr
	}
	if !retry {
		return nil // signal success to stop retrying
	}
	if err != nil {
		return err
	}
	// retry == true && err == nil → retryable status on resp
	if resp != nil {
		c.lastErr = c.rt.retryableStatus(resp)
		return c.lastErr
	}
	c.lastErr = nil
	return nil
}

// exhausted returns the [RetryExhaustedError] of a request whose retry loop
// stopped on a retryable failure because no further attempt was allowed (the
// attempts, the elapsed-time budget or the context deadline ran out). An
// exhausted retryable status is reported as an [UnexpectedStatusError] of
// the last reply, so callers classify it like any other failed status. The
// URL omits the query string and user info, which may carry credentials.
func exhausted(req *http.Request, lastResp *http.Response, retryErr error, attempts int) error {
	last := retryErr
	if _, ok := errors.AsType[*retryableStatusError](retryErr); ok && lastResp != nil && lastResp.Request != nil {
		last = unexpectedStatus(lastResp)
	}
	return &RetryExhaustedError{
		LastError: last,
		Attempts:  attempts,
		Method:    req.Method,
		URL:       redactedURL(req.URL),
	}
}

// unexpectedStatus describes the failed reply resp. A redirect hop's request
// carries no Host field, so the URL's host stands in for it. URI is the
// escaped path without the query string, which may carry credentials.
func unexpectedStatus(resp *http.Response) *UnexpectedStatusError {
	se := &UnexpectedStatusError{Status: resp.StatusCode}
	if req := resp.Request; req != nil {
		se.Method = req.Method
		if req.URL != nil {
			se.Host = cmp.Or(req.Host, req.URL.Host)
			se.URI = cmp.Or(req.URL.EscapedPath(), "/")
		}
	}
	return se
}

// finish turns the outcome of the retry loop, after attempts attempts, into
// the round trip result.
func (rt *retryRoundTripper) finish(lastResp *http.Response, lastErr, retryErr error, attempts int) (*http.Response, error) {
	// Handle retry exhaustion and terminal errors (possibly with a response,
	// e.g. a non-retryable or exhausted retryable status).
	if retryErr != nil {
		if rt.errorHandler != nil {
			resp, err := rt.errorHandler(lastResp, retryErr, attempts)
			// The handler's error is returned: it renders trusted text only,
			// as a transport error does (it may quote resp.Request.URL).
			err = redactTransportError(err)
			if resp != lastResp {
				// The handler did not hand the original response on.
				closeBody(lastResp)
			}
			return discardOnError(resp, err)
		}
		return discardOnError(lastResp, retryErr)
	}

	return discardOnError(lastResp, lastErr)
}

// discardOnError enforces the [http.RoundTripper] contract that a response
// accompanies only a nil error: [http.Client] drops a response returned
// together with an error without closing it, so its body would leak.
func discardOnError(resp *http.Response, err error) (*http.Response, error) {
	if err != nil {
		closeBody(resp)
		return nil, err
	}
	return resp, nil
}

// maxDiscardDrain caps how many bytes of a discarded response body are read
// before closing it. Draining lets HTTP/1.1 reuse the connection; larger
// bodies are cheaper to abandon together with their connection.
const maxDiscardDrain = 64 << 10

// closeBody drains (up to [maxDiscardDrain]) and closes resp's body when
// there is one. Bodies of a larger known length and protocol upgrades are
// closed without reading, and so is every body of a request without a
// deadline: a stalled upstream must not block the caller indefinitely.
//
// The drain stays synchronous although [http.Transport] drains an unread body
// after Close itself (Go 1.27): that drain runs in the background, so a retry
// sent right after Close finds the connection still busy and dials another.
func closeBody(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	if drainable(resp) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDiscardDrain))
	}
	_ = resp.Body.Close()
}

// drainable reports whether resp's discarded body is worth draining.
func drainable(resp *http.Response) bool {
	if resp.StatusCode == http.StatusSwitchingProtocols || resp.ContentLength > maxDiscardDrain || resp.Request == nil {
		return false
	}
	_, ok := resp.Request.Context().Deadline()
	return ok
}

// retryable reports whether err is a failure the retry loop retries: not a
// [NonRetryableError], [CircuitBreakerError], [UnexpectedStatusError] or
// [ResponseSizeError].
func retryable(err error) bool {
	if _, ok := errors.AsType[*NonRetryableError](err); ok {
		return false
	}
	if _, ok := errors.AsType[*CircuitBreakerError](err); ok {
		return false
	}
	if _, ok := errors.AsType[*UnexpectedStatusError](err); ok {
		return false
	}
	_, ok := errors.AsType[*ResponseSizeError](err)
	return !ok
}

// classifyTransportError examines a transport-level error and wraps it as
// non-retryable when appropriate, matching the former retryPolicy logic.
func classifyTransportError(ctx context.Context, err error) error {
	if coreerrs.IsContextCanceledOrDeadlineExceeded(ctx.Err()) {
		return &NonRetryableError{Err: coreerrs.Wrapf(ctx.Err(), "request context error")}
	}

	// An SSRF refusal: the typed error, or any error matching the sentinel
	// (e.g. a caller's dial policy that reports only ErrSSRFBlocked).
	if ssrfRefusal(err) != nil {
		return &NonRetryableError{Err: err}
	}

	// A limiter refusal (WithLimiterPerAttempt): another attempt would be
	// refused or would wait again.
	if errors.Is(err, limiters.ErrRateLimited) {
		return &NonRetryableError{Err: err}
	}

	if permanentTransportError(err) || network.IsResourceRedirects(err) || network.IsConnectionRefused(err) {
		return &NonRetryableError{Err: err}
	}

	if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		var state string
		if errors.Is(err, gobreaker.ErrOpenState) {
			state = "open"
		} else {
			state = "too_many_requests"
		}
		return &CircuitBreakerError{
			Name:  "httpclient",
			State: state,
		}
	}

	if IsResponseSizeError(err) != nil {
		return err
	}

	// The error is likely recoverable so retry.
	return err
}

// permanentTransportError reports a transport failure that another attempt
// cannot fix: a server certificate the client rejects (an unknown authority, a
// host name mismatch, an invalid certificate or any other failed verification)
// or a URL scheme the transport does not support. A round tripper receives
// these errors bare, not inside the [*url.Error] that core/errors'
// IsCertUnknownAuthority and IsUnsupportedProtocolScheme look for, so their
// types are matched directly.
func permanentTransportError(err error) bool {
	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.HostnameError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.CertificateInvalidError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return true
	}
	return unsupportedScheme(err)
}

// classifyResponse examines an HTTP response and returns nil for success, a
// retryable error for the retryable statuses, or an [UnexpectedStatusError]
// for non-retryable failures.
func (rt *retryRoundTripper) classifyResponse(resp *http.Response) error {
	retry, err := classifyStatus(resp, rt.retryableStatuses)
	if retry {
		return rt.retryableStatus(resp)
	}
	return err
}

// ssrfRefusal returns err when it is an SSRF refusal — the typed error, or any
// error matching the sentinel (a caller dial policy may report only
// ErrSSRFBlocked) — and nil otherwise. A refusal is never retried.
func ssrfRefusal(err error) error {
	if err != nil && (IsSSRFError(err) != nil || errors.Is(err, ErrSSRFBlocked)) {
		return err
	}
	return nil
}
