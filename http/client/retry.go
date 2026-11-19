// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
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
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/altessa-s/go-atlas-core/errors/network"
	"github.com/altessa-s/go-atlas-core/time/clock"

	"github.com/altessa-s/go-atlas-net/http/client/limiters"
	"github.com/altessa-s/go-atlas-net/httpx"

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
		e.retryAfter, _ = httpx.ParseRetryAfter(resp.Header, rt.now(), rt.retryAfterMax)
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

// attemptContext returns the context of one attempt: ctx bounded by the
// attempt timeout, with its cancel func, or ctx itself and nil without one.
func (rt *retryRoundTripper) attemptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if rt.attemptTimeout <= 0 {
		return ctx, nil
	}

	return context.WithTimeout(ctx, rt.attemptTimeout)
}

// attemptRequest prepares req, an attempt's private request, for sending:
// with a cookie jar, its Cookie header becomes the caller's original one
// (callerCookie, captured before any attempt) plus the jar's cookies for the
// URL read now, edited in place.
func (rt *retryRoundTripper) attemptRequest(req *http.Request, callerCookie []string) *http.Request {
	out := req
	if rt.jar != nil {
		out.Header.Del("Cookie")

		for _, v := range callerCookie {
			out.Header.Add("Cookie", v)
		}

		for _, c := range rt.jar.Cookies(cookieURL(req)) {
			out.AddCookie(c)
		}
	}

	return out
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

// addCookieNames adds the names of the cookies resp sets to set: those
// recorded on its request view when the reply was received (see
// responseView), or else those its header sets now.
func addCookieNames(set map[string]struct{}, resp *http.Response) {
	if resp == nil {
		return
	}

	if resp.Request != nil {
		if names, ok := resp.Request.Context().Value(receivedCookiesKey{}).([]string); ok {
			for _, name := range names {
				set[name] = struct{}{}
			}

			return
		}
	}

	for _, c := range resp.Cookies() {
		set[c.Name] = struct{}{}
	}
}

// receivedCookiesKey is the context key of the names of the cookies a reply
// set as received, carried by the reply's request view.
type receivedCookiesKey struct{}

// cookieNameList returns the cookie names recorded for the redirect
// filtering, sorted.
func (c *retryCall) cookieNameList() []string {
	return slices.Sorted(maps.Keys(c.cookieNames))
}

// withCookieNames returns a copy of req (a fresh view of the last attempt
// when nil) whose context records the cookie names of this call's replies.
func (c *retryCall) withCookieNames(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), receivedCookiesKey{}, c.cookieNameList()))
}

// viewSource is the request the returned reply's view is built from: the
// last attempt's (recorded before it was sent, so a reply an ErrorHandler
// makes after a transport error has one too), or the snapshot when no
// attempt was made (a buffering failure).
func (c *retryCall) viewSource() *http.Request {
	if c.lastAttemptReq != nil {
		return c.lastAttemptReq
	}

	return c.base
}

// settleView gives the reply going back to http.Client a usable request view.
// The reply received (lastResp) gets a fresh view: whatever a policy or the
// ErrorHandler did to the one they saw (its URL, its context and the cookie
// names recorded there), the redirect handling reads the library's own. An
// ErrorHandler's own reply without a usable request (a redirect among them)
// gets a fresh view too; one with a request carries, with a cookie jar, the
// cookie names this hop's replies set, for the redirect filtering.
func (c *retryCall) settleView(resp, lastResp *http.Response) {
	switch {
	case resp == nil:
	case resp == lastResp, resp.Request == nil, resp.Request.URL == nil:
		resp.Request = c.responseView(c.viewSource(), c.cookieNameList())
	case c.rt.jar != nil:
		resp.Request = c.withCookieNames(resp.Request)
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

	if err != nil || resp == nil {
		cancel()
		return resp
	}

	// attachCancel keeps a writable (101 Switching Protocols) body writable.
	return attachCancel(resp, cancel)
}

// CloseIdleConnections forwards to the wrapped transport so
// [http.Client.CloseIdleConnections] releases pooled connections.
func (rt *retryRoundTripper) CloseIdleConnections() { closeIdleConnections(rt.next) }

// RoundTrip implements [http.RoundTripper]. It takes a private snapshot of
// the request (method, URL, header, framing) before the first transmission
// and builds every attempt from it, buffering the body privately when it must
// be replayed and has no GetBody; the caller's request is never modified.
// Retry orchestration is delegated to [coreretry.Do]. Every call reports
// exactly one RequestDone to the observer, timed from the start of the call
// (buffering included).
func (rt *retryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	start := rt.now()

	if refused := refuseAmbiguousIdempotencyKey(req); refused != nil {
		rt.requestDone(req, start, "unknown", 0, true)
		return nil, refused
	}

	ctx := req.Context()

	base := snapshotRequest(req)
	call := &retryCall{
		rt:   rt,
		req:  req,
		ctx:  ctx,
		base: base,
		// The caller's Cookie header as received, from the snapshot: with a
		// jar the retry layer owns cookie handling (http.Client.Jar is nil),
		// so nothing was injected; on a redirect hop it was already filtered
		// by redirectPolicy.
		callerCookie: base.Header.Values("Cookie"),
		firstAttempt: true,
		firstBody:    req.Body,
		getBody:      req.GetBody,
	}

	// An eligible request gets a replayable body (buffered when it has no
	// GetBody) whether or not it is marked with ContextWithoutRetry: the mark
	// only caps the retry loop at one call to the next round tripper, and the
	// transport may still rewind the body itself.
	eligible := replayable(req)
	replay := eligible && !RetryDisabled(ctx)

	if eligible {
		getBody, buffered, bufErr := rt.bufferBody(req)
		if bufErr != nil {
			// A buffering failure (an ended context, a failing body) ends the
			// call before any attempt: it takes the common terminal path, so
			// the ErrorHandler sees it with zero attempts, as it does when the
			// context ended before the first attempt of a body it need not
			// buffer.
			resp, handles, err := rt.finish(nil, nil, bufErr, 0)
			resp, err = call.settle(resp, err, handles)
			call.settleView(resp, nil)
			rt.requestDone(call.base, start, "unknown", 0, true)

			return resp, err
		}

		if buffered {
			// The caller's body was read and closed by buffering; every
			// attempt reads a private copy.
			call.getBody = getBody
			call.firstBody, _ = getBody()
			call.callerBodyClosed = true
			// Buffering read the body to its end, so the trailers are final:
			// every attempt sends its own copy of them.
			call.setFinalTrailers(req.Trailer.Clone())
		}
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
	if !call.bodyHanded && !call.callerBodyClosed && req.Body != nil {
		// No attempt reached the next round tripper (the context ended before
		// the first one): the caller's request body is still ours to close,
		// as http.RoundTripper requires.
		_ = req.Body.Close()
	}

	statusClass := "unknown"
	if call.lastStatus != 0 {
		statusClass = statusToClass(call.lastStatus)
	}

	if retryErr != nil && ctx.Err() == nil && retryable(retryErr) {
		retryErr = exhausted(call.base, call.lastUnexpected, retryErr, call.attempts)
	}
	// A pass-through policy (WithoutRetry) clears retryErr but leaves the
	// attempt's error in lastErr, which finish returns: still a failure.
	failed := retryErr != nil || call.lastErr != nil
	lastResp := call.lastResp

	resp, handles, err := rt.finish(call.lastResp, call.lastErr, retryErr, call.attempts)
	resp, err = call.settle(resp, err, handles)

	call.settleView(resp, lastResp)
	// Retries count the calls actually made to the next round tripper.
	rt.requestDone(call.base, start, statusClass, max(call.attempts-1, 0), failed)

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

// replayable reports whether the retry layer may send req more than once:
// an idempotent method of RFC 9110 (GET, HEAD, OPTIONS, TRACE, PUT, DELETE;
// method names are case-sensitive, an empty method is GET) or a request
// carrying exactly one non-blank idempotency key (see [idempotencyKey]).
// Any other request (POST, PATCH, a lowercase "post", ...) is attempted once,
// so a server that processed it before failing never sees it twice. The rule
// holds under a [RetryPolicyHandler] too: the policy classifies the outcome of
// that one attempt but cannot ask for another.
func replayable(req *http.Request) bool {
	if idempotentMethod(req.Method) {
		return true
	}

	_, single := idempotencyKey(req.Header)

	return single
}

// idempotentMethod reports whether method is idempotent per RFC 9110: GET,
// HEAD, OPTIONS, TRACE, PUT or DELETE, matched case-sensitively; an empty
// method is GET.
func idempotentMethod(method string) bool {
	switch method {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodPut, http.MethodDelete:
		return true
	}

	return false
}

// ambiguousIdempotencyKey reports whether req, a request of a non-idempotent
// method, carries an idempotency key header that is not exactly one
// non-blank value (see [ErrAmbiguousIdempotencyKey]).
func ambiguousIdempotencyKey(req *http.Request) bool {
	if idempotentMethod(req.Method) {
		return false
	}

	present, single, carrier := effectiveIdempotencyKey(req.Header)

	return present && (!single || connectionNames(req.Header, carrier))
}

// connectionNames reports whether a Connection header of h (under any map key
// that matches it ASCII case-insensitively; every value, comma-separated,
// tokens trimmed) names the header field name: such a field is hop-by-hop, so
// an intermediary drops it and the server may not see the key the request is
// replayed under. A token naming the other key header, which carries no
// value, is harmless.
func connectionNames(h http.Header, name string) bool {
	for key, values := range h {
		if !asciiEqualFold(key, "Connection") {
			continue
		}

		for _, v := range values {
			for token := range strings.SplitSeq(v, ",") {
				token = strings.Trim(token, " \t")
				if asciiEqualFold(token, name) {
					return true
				}
			}
		}
	}

	return false
}

// Idempotency key header names, in their canonical form.
const (
	headerIdempotencyKey  = "Idempotency-Key"
	headerXIdempotencyKey = "X-Idempotency-Key"
)

// refuseAmbiguousIdempotencyKey returns [ErrAmbiguousIdempotencyKey] for a
// request [ambiguousIdempotencyKey] reports, after closing its body as
// [http.RoundTripper] requires, and nil for any other request. The refusal is
// terminal: no attempt is made and no [ErrorHandler] is consulted.
func refuseAmbiguousIdempotencyKey(req *http.Request) error {
	if !ambiguousIdempotencyKey(req) {
		return nil
	}

	if req.Body != nil {
		_ = req.Body.Close()
	}

	return ErrAmbiguousIdempotencyKey
}

// idempotencyKeyGuard is the outermost round tripper of the client: it
// refuses a request with an ambiguous idempotency key before the limiter and
// the retry layer see it, reporting it to the observer as a failed request of
// zero attempts.
type idempotencyKeyGuard struct {
	next http.RoundTripper
	rt   *retryRoundTripper
}

// RoundTrip implements [http.RoundTripper].
func (g idempotencyKeyGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	start := g.rt.now()
	if refused := refuseAmbiguousIdempotencyKey(req); refused != nil {
		g.rt.requestDone(req, start, "unknown", 0, true)
		return nil, refused
	}

	return g.next.RoundTrip(req)
}

// CloseIdleConnections forwards to the wrapped transport.
func (g idempotencyKeyGuard) CloseIdleConnections() { closeIdleConnections(g.next) }

// idempotencyKey inspects the idempotency key of h: the values of
// Idempotency-Key and X-Idempotency-Key under every map key that matches
// either name ASCII case-insensitively (header names are ASCII tokens; no
// Unicode folding). present reports such a map key, even with no value;
// single reports exactly one value in total that is not empty once HTTP
// whitespace (SP, HTAB) is trimmed. The caller's slices are only read.
func idempotencyKey(h http.Header) (present, single bool) {
	present, single, _ = effectiveIdempotencyKey(h)
	return present, single
}

// effectiveIdempotencyKey is [idempotencyKey] that also returns, when single,
// the canonical name of the header carrying the key (Idempotency-Key or
// X-Idempotency-Key).
func effectiveIdempotencyKey(h http.Header) (present, single bool, carrier string) {
	var (
		count int
		value string
	)

	for name, values := range h {
		canonical := headerIdempotencyKey
		if !asciiEqualFold(name, headerIdempotencyKey) {
			if !asciiEqualFold(name, headerXIdempotencyKey) {
				continue
			}

			canonical = headerXIdempotencyKey
		}

		present = true
		count += len(values)

		if len(values) == 1 {
			value, carrier = values[0], canonical
		}
	}

	if count != 1 || strings.Trim(value, " \t") == "" {
		return present, false, ""
	}

	return present, true, carrier
}

// asciiEqualFold reports whether s and t are equal under ASCII case folding
// only, so a non-ASCII rune (the Kelvin sign, for example) never matches a
// letter of a header name.
func asciiEqualFold(s, t string) bool {
	if len(s) != len(t) {
		return false
	}

	for i := range len(s) {
		if lowerASCII(s[i]) != lowerASCII(t[i]) {
			return false
		}
	}

	return true
}

// lowerASCII lowers an ASCII upper-case letter and returns any other byte as is.
func lowerASCII(b byte) byte {
	if 'A' <= b && b <= 'Z' {
		return b + ('a' - 'A')
	}

	return b
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
//
// It returns buffered false, leaving the body unread, when no buffering is
// needed; otherwise getBody returns a fresh reader over the private copy.
func (rt *retryRoundTripper) bufferBody(req *http.Request) (getBody func() (io.ReadCloser, error), buffered bool, err error) {
	if req.Body == nil || isNoBody(req.Body) || req.GetBody != nil || rt.maxAttempts == 0 {
		return nil, false, nil
	}

	ctx := req.Context()

	closeBody := sync.OnceValue(req.Body.Close)
	if ctxErr := ctx.Err(); ctxErr != nil {
		_ = closeBody()
		return nil, false, ctxErr
	}

	stop := context.AfterFunc(ctx, func() { _ = closeBody() })
	buf := coreio.GetBuffer()
	_, err = buf.ReadFrom(req.Body)

	stop()

	_ = closeBody()

	if err != nil {
		coreio.PutBuffer(buf)

		if ctxErr := ctx.Err(); ctxErr != nil {
			// The read failed because the context ended and closed the body.
			return nil, false, ctxErr
		}
		// The body's error text is the caller's: render a trusted one.
		return nil, false, coreerrs.Wrapf(redactTransportError(err), "failed to read request body for retry buffering")
	}

	bodyBytes := bytes.Clone(buf.Bytes())
	coreio.PutBuffer(buf)

	// The copy stays private to the retry call: the caller's request keeps
	// its fields, and every attempt keeps the original framing (its
	// ContentLength, so an unknown length is still sent chunked).
	return func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(bodyBytes)), nil
	}, true, nil
}

// snapshotRequest returns the private template every attempt of req is built
// from: a deep copy of its method, URL, header, trailer and framing, taken
// before the first transmission, without its body.
func snapshotRequest(req *http.Request) *http.Request {
	base := req.Clone(req.Context())
	base.Body = nil
	base.GetBody = nil

	return base
}

// errRequestChanged stops a retry when the request handed to the retry layer
// changed after the snapshot its attempts are built from (a policy, an
// observer or a callback edited it): the retry layer neither resends the
// snapshot nor sends the edited request.
var errRequestChanged = errors.New("request changed during the retry loop; not replayed")

// requestChanged reports whether req, the request handed to the retry layer,
// differs from its snapshot base in one of the compared fields: Method, URL,
// Host, Header, ContentLength or TransferEncoding. Body, GetBody and Trailer
// are not compared (a streaming body fills its trailers as it is sent).
func requestChanged(req, base *http.Request) bool {
	return req.Method != base.Method || req.Host != base.Host || !sameURL(req.URL, base.URL) ||
		req.ContentLength != base.ContentLength || !slices.Equal(req.TransferEncoding, base.TransferEncoding) ||
		!maps.EqualFunc(req.Header, base.Header, slices.Equal[[]string])
}

// sameURL reports whether a and b are equal field by field (Host included
// when Opaque is set, which URL.String omits); nil equals only nil.
func sameURL(a, b *url.URL) bool {
	if a == nil || b == nil {
		return a == b
	}

	return a.Scheme == b.Scheme && a.Opaque == b.Opaque && a.Host == b.Host && a.Path == b.Path &&
		a.RawPath == b.RawPath && a.OmitHost == b.OmitHost && a.ForceQuery == b.ForceQuery &&
		a.RawQuery == b.RawQuery && a.Fragment == b.Fragment && a.RawFragment == b.RawFragment &&
		sameUserinfo(a.User, b.User)
}

// sameUserinfo reports whether a and b carry the same user name and password.
func sameUserinfo(a, b *url.Userinfo) bool {
	if a == nil || b == nil {
		return a == b
	}

	ap, aok := a.Password()
	bp, bok := b.Password()

	return a.Username() == b.Username() && ap == bp && aok == bok
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
	// attempts counts the attempts handed to the next round tripper so far.
	attempts     int
	firstAttempt bool
	// bodyHanded reports whether an attempt handed the request body to the
	// next round tripper, which then owns closing it.
	bodyHanded bool
	// base is the private snapshot every attempt is built from.
	base *http.Request
	// firstBody is the body of the first attempt (the caller's, or a private
	// copy when the body was buffered); getBody rewinds it for a retry (the
	// caller's GetBody or the private buffer), nil when it cannot.
	firstBody io.ReadCloser
	getBody   func() (io.ReadCloser, error)
	// callerBodyClosed reports that buffering already read and closed the
	// caller's body.
	callerBodyClosed bool
	// trailerMu guards finalTrailer and trailerFinal: a body reaches EOF on
	// the transport's writing goroutine.
	trailerMu sync.Mutex
	// lastAttemptReq is the request of the last reply received and
	// cookieNames the names of the cookies the replies of this call set (or
	// deleted) as received, with a cookie jar:
	// the reply returned to http.Client gets a fresh view built from them.
	lastAttemptReq *http.Request
	cookieNames    map[string]struct{}
	// owned is the body of the last reply, owned by the attempt until it is
	// disposed of (dispose) or handed back to the caller. lastContentLength,
	// lastRetryable and lastUnexpected record that reply as received.
	owned             *bodyHandle
	lastContentLength int64
	lastRetryable     *retryableStatusError
	lastUnexpected    *UnexpectedStatusError
	// publishMu serializes trailer publication across the call's bodies.
	publishMu sync.Mutex
	// liveBodies holds a close signal for every body that may write the
	// caller's live trailers: each attempt's body and each body the transport
	// got from the attempt's GetBody (see awaitLiveBodies). Guarded by
	// trailerMu: the transport calls GetBody on its own goroutines.
	liveBodies []chan struct{}
	// finalTrailer holds the request's trailers once final (trailerFinal):
	// after buffering, or when a transmission of a streaming body read it to
	// its end (the caller's live map, filled by the body, is cloned then).
	// Every attempt from then on sends its own copy, so an edit to one
	// attempt's trailers never reaches the next.
	finalTrailer http.Header
	trailerFinal bool
	// lastStatus is the status of the last reply received, kept whatever
	// happens to the reply's body.
	lastStatus int
}

// captureTrailersAtEOF wraps body so the caller's live trailers are captured
// as final when it is read to its end.
func (c *retryCall) captureTrailersAtEOF(body io.ReadCloser, attemptTrailer http.Header, probeable bool) *eofHookBody {
	// capture runs on the goroutine that read the body to its end, which
	// also filled the caller's map: it records that map as final. It never
	// touches the attempt's map.
	capture := c.captureLiveTrailers
	// publish brings the attempt's map up to date with the caller's: only
	// the values of keys declared before sending (the key set is fixed; Go
	// sends only predeclared trailers), only where they differ. Static
	// trailers are left untouched. See eofHookBody for when it may run.
	publish := func() {
		// One publication at a time across all bodies of the call: two
		// bodies of one attempt (the original and a transport rewind) share
		// its map.
		c.publishMu.Lock()
		defer c.publishMu.Unlock()

		for k, v := range attemptTrailer {
			if live := c.req.Trailer[k]; !slices.Equal(v, live) {
				attemptTrailer[k] = slices.Clone(live)
			}
		}
	}
	hooked := &eofHookBody{
		ReadCloser: body, capture: capture, publish: publish, probeable: probeable, closed: make(chan struct{}),
	}

	c.trailerMu.Lock()
	c.liveBodies = append(c.liveBodies, hooked.closed)
	c.trailerMu.Unlock()

	return hooked
}

// captureLiveTrailers records a copy of the caller's live trailers as final,
// cloned under the lock, unless they are already known.
func (c *retryCall) captureLiveTrailers() {
	c.trailerMu.Lock()
	defer c.trailerMu.Unlock()

	if !c.trailerFinal {
		c.finalTrailer, c.trailerFinal = c.req.Trailer.Clone(), true
	}
}

// awaitLiveBodies waits, bounded by ctx, until every body that may write the
// caller's trailers is closed with no Read in flight: until then the
// transport may still read one and fill those trailers, which the next
// attempt must not share, and the caller's GetBody must not be asked for
// another body.
func (c *retryCall) awaitLiveBodies(ctx context.Context) error {
	c.trailerMu.Lock()
	bodies := slices.Clone(c.liveBodies)
	c.trailerMu.Unlock()

	for _, closed := range bodies {
		select {
		case <-closed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

// responseView returns the request a reply carries to policies, the
// ErrorHandler and the caller: a metadata-only copy of req with no Body or
// GetBody and its own Header, URL and TransferEncoding. Its Trailer is a copy
// of the final trailers when known, or else of the declaration captured
// before the first transmission: the live map may still be written by an
// upload in progress (an early or full-duplex reply), so it is never read
// here. Inspecting or editing the view touches no request being or to be sent.
//
// With a cookie jar the view's context also records the names of the cookies
// resp set as received, for the redirect cookie filtering
// (filterRedirectCookies): editing the reply's header cannot change them.
func (c *retryCall) responseView(req *http.Request, cookieNames []string) *http.Request {
	ctx := req.Context()
	if c.rt.jar != nil {
		ctx = context.WithValue(ctx, receivedCookiesKey{}, slices.Clone(cookieNames))
	}

	// A shallow staging copy drops what the view must not share or expose
	// (the live trailer map, the body, the redirect history, which belongs to
	// http.Client) before Request.Clone deep-copies the rest: header, URL
	// with its user info, TransferEncoding, forms and path values.
	staging := req.WithContext(ctx)
	staging.Trailer = nil
	staging.Body = nil
	staging.GetBody = nil
	staging.Response = nil

	view := staging.Clone(ctx)

	if final, ok := c.finalTrailers(); ok {
		view.Trailer = final.Clone()
	} else {
		view.Trailer = c.base.Trailer.Clone()
	}

	return view
}

// finalTrailers returns the final trailers and whether they are known.
func (c *retryCall) finalTrailers() (http.Header, bool) {
	c.trailerMu.Lock()
	defer c.trailerMu.Unlock()

	return c.finalTrailer, c.trailerFinal
}

// setFinalTrailers records t as the final trailers, unless already known.
func (c *retryCall) setFinalTrailers(t http.Header) {
	c.trailerMu.Lock()
	defer c.trailerMu.Unlock()

	if !c.trailerFinal {
		c.finalTrailer, c.trailerFinal = t, true
	}
}

// eofHookBody calls onEOF once, when its body is read to its end, and closes
// closed once the body is closed.
//
// At the body's end it captures the caller's trailers (capture) and brings the
// attempt's trailer map up to date (publish) before EOF reaches the
// transport, which writes the trailers only after reading EOF. A Read that
// returns the last data together with EOF is split: the data goes at once,
// and the next Read publishes and reports the end. One case is deferred: an
// end met by what may be net/http's probe, a first one-byte Read of a request
// its transferWriter may probe (unknown length, a GET, HEAD, DELETE, OPTIONS,
// PROPFIND or SEARCH): the probe runs on its own goroutine while the request
// headers, which enumerate the trailer keys, are written, so a write to the
// map then would race with that enumeration. The publication waits for the
// next Read or for Close, both of which net/http makes after the headers (the
// transport reads the probed body again before writing a chunked body's
// trailers, and closes it before writing them).
type eofHookBody struct {
	io.ReadCloser
	capture func()
	publish func()

	// probeable reports a request net/http's transferWriter may probe: a
	// method that usually has no body, an unknown length, no
	// Transfer-Encoding. Its probe is a first Read of one byte.
	probeable bool

	mu           sync.Mutex
	eofNext      bool // the next Read reports the end
	calls        int  // Reads begun
	reads        int  // Reads in flight
	ended        bool // the end was met
	pending      bool // publish deferred
	isClosedFlag bool // Close was called
	closed       chan struct{}
	closeOnce    sync.Once
}

// Close closes the body, makes a deferred publication, and signals closed once
// no Read is in flight.
func (b *eofHookBody) Close() error {
	err := b.ReadCloser.Close()

	b.mu.Lock()
	b.publishPending()
	b.isClosedFlag = true
	b.signalIdle()
	b.mu.Unlock()

	return err
}

// Read reads from the body; at its end it captures and publishes the
// trailers (see eofHookBody).
func (b *eofHookBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	b.publishPending()

	if b.eofNext {
		// The previous Read returned the body's last data: the end, with the
		// trailers just published, is reported now.
		b.eofNext = false
		b.mu.Unlock()

		return 0, io.EOF
	}

	b.calls++
	first := b.calls == 1
	b.reads++
	b.mu.Unlock()

	n, err := b.ReadCloser.Read(p)

	b.mu.Lock()
	if err == io.EOF && !b.ended { //nolint:errorlint // io.Reader returns io.EOF itself, never wrapped
		b.ended = true
		b.capture()

		switch {
		case n > 0:
			// Data with the end: the data goes now, the end on the next Read,
			// which publishes first. A transport writes the trailers only
			// after it has seen the end.
			b.pending, b.eofNext = true, true
			err = nil
		case first && len(p) == 1 && b.probeable:
			// Possibly net/http's probe, concurrent with the header write.
			b.pending = true
		default:
			b.publish()
		}
	}

	b.reads--
	b.signalIdle()
	b.mu.Unlock()

	return n, err
}

// isClosed reports whether Close was called.
func (b *eofHookBody) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.isClosedFlag
}

// publishPending makes a deferred publication. b.mu is held.
func (b *eofHookBody) publishPending() {
	if b.pending {
		b.pending = false
		b.publish()
	}
}

// signalIdle closes closed once the body is closed and no Read is in flight.
// b.mu is held.
func (b *eofHookBody) signalIdle() {
	if b.isClosedFlag && b.reads == 0 {
		b.closeOnce.Do(func() { close(b.closed) })
	}
}

// probeableRequest reports whether net/http's transferWriter may probe req's
// body: a method that usually has no body, an unknown length (ContentLength
// 0 or -1 with a body) and no Transfer-Encoding.
func probeableRequest(req *http.Request) bool {
	switch req.Method {
	case "", http.MethodGet, http.MethodHead, http.MethodDelete, http.MethodOptions, "PROPFIND", "SEARCH":
	default:
		return false
	}

	return req.ContentLength <= 0 && len(req.TransferEncoding) == 0
}

// prepareTrailers gives attempt its own trailer map: a copy of the final
// trailers when known, else of the declaration made before the first
// transmission. The transport never sees the caller's map. A caller body
// (its original or a GetBody result, the transport's rewinds included) fills
// the caller's map as it is read; a hook copies those values into the
// attempt's map at the body's end, before the transport writes the trailers.
// Every such body is tracked (see awaitLiveBodies): two of them must not
// write the caller's map at once.
func (c *retryCall) prepareTrailers(attempt *http.Request) {
	if final, ok := c.finalTrailers(); ok {
		attempt.Trailer = final.Clone()
	} else {
		attempt.Trailer = c.base.Trailer.Clone()
	}

	if c.req.Trailer == nil || c.callerBodyClosed || attempt.Body == nil || isNoBody(attempt.Body) {
		return // no trailers, or a private buffered copy that writes none
	}

	own := attempt.Trailer
	probeable := probeableRequest(attempt)
	original := c.captureTrailersAtEOF(attempt.Body, own, probeable)
	attempt.Body = original

	if c.getBody != nil {
		ctx := attempt.Context()
		attempt.GetBody = func() (io.ReadCloser, error) {
			// A transport may ask for a copy while the attempt's body is
			// still open (and read the copy first): no wait then. Once it
			// closed that body to rewind, the copy waits, bounded by the
			// attempt, until no Read of it is in flight. Earlier attempts'
			// bodies were waited for before this attempt began.
			if original.isClosed() {
				select {
				case <-original.closed:
				case <-ctx.Done():
				}
			}

			if err := ctx.Err(); err != nil {
				return nil, err
			}

			body, err := c.getBody()
			if err != nil || body == nil || isNoBody(body) {
				return body, err
			}

			return c.captureTrailersAtEOF(body, own, probeable), nil
		}
	}
}

// onRetry runs before each retry's backoff sleep.
func (c *retryCall) onRetry(attempt int, err error, delay time.Duration) {
	// A retry follows: release the discarded response before the
	// backoff sleep so its connection returns to the pool meanwhile.
	// With an ErrorHandler the response must stay readable in case
	// the context ends during the sleep and the handler receives it.
	if c.rt.errorHandler == nil && c.lastResp != nil && c.lastResp.Body != nil {
		c.dispose(c.lastResp)
		c.lastResp.Body = http.NoBody
	}

	c.rt.observer.Retry(AttemptEvent{Method: c.base.Method, Host: c.base.URL.Hostname()})

	if c.rt.logger != nil {
		c.rt.logger.DebugContext(c.ctx,
			"http client retrying request",
			slog.Int("attempt", attempt),
			slog.String("method", c.base.Method),
			// Omit user info and the query string: outbound URLs may
			// carry signed-URL credentials or tokens that must not reach
			// debug logs. err renders trusted text only (transportError).
			slog.String("url", redactedURL(c.base.URL)),
			slog.String("error", err.Error()),
			slog.Duration("delay", delay),
		)
	}
}

// try makes one attempt of the round trip and classifies its outcome for
// coreretry.Do.
func (c *retryCall) try(ctx context.Context) error {
	// Close previous response body if present from a prior attempt.
	c.dispose(c.lastResp)
	c.lastResp = nil

	// A retry stops when the caller's request changed after the snapshot:
	// neither the old request nor the edited one is sent.
	if !c.firstAttempt && requestChanged(c.req, c.base) {
		c.lastErr = errRequestChanged
		return &NonRetryableError{Err: c.lastErr}
	}

	// Every attempt is a fresh copy of the private snapshot: what a policy
	// or the next round tripper does to an attempt never reaches the next.
	attempt := c.base.Clone(ctx)
	attempt.Body = c.firstBody

	hasBody := c.firstBody != nil && !isNoBody(c.firstBody)
	if !c.firstAttempt && hasBody {
		// No body may still write the live trailers when the caller's
		// GetBody is asked for the next one.
		if err := cmp.Or(c.awaitLiveBodies(ctx), ctx.Err()); err != nil {
			c.lastErr = err
			return &NonRetryableError{Err: err}
		}

		if c.getBody == nil {
			// Unreachable: a replayed body is buffered when it has no GetBody.
			c.lastErr = errors.New("request body cannot be rewound for retry")
			return &NonRetryableError{Err: c.lastErr}
		}

		body, gbErr := c.getBody()
		if gbErr != nil {
			c.lastErr = coreerrs.Wrapf(redactTransportError(gbErr), "failed to rewind request body for retry")
			return &NonRetryableError{Err: c.lastErr}
		}

		attempt.Body = body
	}

	attempt.GetBody = c.getBody

	// The attempt's own context (its deadline) exists before the trailer
	// hooks are installed: a transport rewind waits under it.
	actx, cancel := c.rt.attemptContext(ctx)
	attempt = attempt.WithContext(actx)

	c.prepareTrailers(attempt)

	c.firstAttempt = false

	attemptReq := c.rt.attemptRequest(attempt, c.callerCookie)
	// Recorded before sending: a reply an ErrorHandler makes after a
	// transport error gets its view from it.
	c.lastAttemptReq = attemptReq
	c.attempts++
	c.bodyHanded = true
	resp, err := c.rt.next.RoundTrip(attemptReq) //nolint:bodyclose // resp is stored in lastResp and either closed on next retry or returned to caller
	// From here on a transport error renders trusted text only: its own
	// text may embed the request URL with its query string.
	err = redactTransportError(err)
	resp = releaseWithBody(resp, err, cancel) //nolint:bodyclose // same response, closed like resp above

	if resp != nil {
		// The attempt owns the body it received, whatever callbacks later do
		// to resp.Body; its outcome is recorded before any callback runs.
		resp.Body = c.own(resp)
		c.lastStatus = resp.StatusCode
		c.lastContentLength = resp.ContentLength
		c.lastRetryable = c.rt.retryableStatus(resp)
		c.lastUnexpected = attemptStatus(attemptReq, c.base, resp.StatusCode)
	}

	// Every reply's cookies are stored exactly once, on receipt, whatever
	// happens to the reply next (retried, returned, discarded).
	c.rt.storeCookies(c.base, resp)

	if resp != nil {
		// Policies, the ErrorHandler and the caller see a detached view of
		// the attempt: editing it reaches neither the request the transport
		// may still be sending (an early reply) nor the next attempt.
		if c.rt.jar != nil {
			// Names set or deleted by every reply of this hop, discarded
			// attempts included, so a later reply that sets none does not
			// hide them. They are kept even when this hop carries no Cookie
			// header: a cross-domain hop's request has its cookies stripped,
			// yet the names its replies set still count for a later hop
			// back on the initial domain (see filterRedirectCookies).
			if c.cookieNames == nil {
				c.cookieNames = make(map[string]struct{})
			}

			for _, cookie := range resp.Cookies() {
				c.cookieNames[cookie.Name] = struct{}{}
			}
		}

		resp.Request = c.responseView(attemptReq, c.cookieNameList())
	}

	c.lastResp = resp
	c.lastErr = err

	// A breaker rejection is reported from the attempt's own outcome, once,
	// before any classification: a retry policy (WithoutRetry's pass-through
	// among them) may replace or suppress the error the caller then gets.
	if IsCircuitBreakerOpen(err) {
		c.rt.observer.BreakerRejected(AttemptEvent{Method: c.base.Method, Host: c.base.URL.Hostname()})
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
	if _, ok := errors.AsType[*retryableStatusError](classified); ok {
		classified = c.lastRetryable
	}

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
	// retry == true && err == nil → retryable status on resp, as received
	// (the policy may have edited resp)
	if resp != nil {
		c.lastErr = c.lastRetryable
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
func exhausted(req *http.Request, lastStatus *UnexpectedStatusError, retryErr error, attempts int) error {
	last := retryErr
	if _, ok := errors.AsType[*retryableStatusError](retryErr); ok && lastStatus != nil {
		last = lastStatus
	}

	return &RetryExhaustedError{
		LastError: last,
		Attempts:  attempts,
		Method:    req.Method,
		URL:       redactedURL(req.URL),
	}
}

// attemptStatus describes a failed reply of status to attemptReq, from the
// attempt's private request (base supplies the method), as received: what a
// callback later does to the reply or its request view does not change it.
func attemptStatus(attemptReq, base *http.Request, status int) *UnexpectedStatusError {
	se := &UnexpectedStatusError{Status: status, Method: base.Method}
	if u := attemptReq.URL; u != nil {
		se.Host = cmp.Or(attemptReq.Host, u.Host)
		se.URI = cmp.Or(u.EscapedPath(), "/")
	}

	return se
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
// the round trip result, before its bodies are settled (see settle). The
// ErrorHandler receives the reply with its body behind a close-once handle of
// its own (handed); that handle, and one on any body the handler then put on
// that reply, are returned for the settlement.
func (rt *retryRoundTripper) finish(
	lastResp *http.Response, lastErr, retryErr error, attempts int,
) (*http.Response, []*bodyHandle, error) {
	// Handle retry exhaustion and terminal errors (possibly with a response,
	// e.g. a non-retryable or exhausted retryable status).
	if retryErr != nil {
		if rt.errorHandler != nil {
			var handed *bodyHandle
			if lastResp != nil && lastResp.Body != nil {
				handed = &bodyHandle{ReadCloser: lastResp.Body}
				lastResp.Body = withWriteSide(handed, lastResp.Body)
			}

			resp, err := rt.errorHandler(lastResp, retryErr, attempts)

			handles := []*bodyHandle{handed}
			if lastResp != nil && lastResp.Body != nil && handleOf(lastResp.Body) != handed {
				// The handler put a body of its own on the reply it was given:
				// it is settled too, behind a close-once handle of its own.
				leftover := &bodyHandle{ReadCloser: lastResp.Body}
				lastResp.Body = withWriteSide(leftover, lastResp.Body)
				handles = append(handles, leftover)
			}

			// The handler's error is returned: it renders trusted text only,
			// as a transport error does (it may quote resp.Request.URL).
			return resp, handles, redactTransportError(err)
		}

		return lastResp, nil, retryErr
	}

	return lastResp, nil, lastErr
}

// settle disposes of the bodies of the round trip's outcome, enforcing the
// [http.RoundTripper] contract that a response accompanies only a nil error
// ([http.Client] drops a response returned with an error without closing
// it). Ownership is decided only by the identity of the library's own
// close-once handles (bodyHandle), never by comparing bodies: the body the
// last attempt received (owned), the handle the ErrorHandler got (handed),
// and whatever body is returned. With an error every one is released, each
// handle once. Without, the returned body passes to the caller, and the
// handles it is not are released when it is closed (it may wrap them), or at
// once when no body is returned.
func (c *retryCall) settle(resp *http.Response, err error, handles []*bodyHandle) (*http.Response, error) {
	if err != nil {
		if resp != nil {
			c.discardBody(resp.Body)
		}

		for _, h := range handles {
			if h != nil {
				_ = h.Close()
			}
		}

		c.releaseOwned()

		return nil, err
	}

	c.handOver(resp, handles)

	return resp, nil
}

// maxDiscardDrain caps how many bytes of a discarded response body are read
// before closing it. Draining lets HTTP/1.1 reuse the connection; larger
// bodies are cheaper to abandon together with their connection.
const maxDiscardDrain = 64 << 10

// bodyHandle is a close-once pointer handle on a body: the body an attempt
// received (owned), or the one the ErrorHandler is given. The library tells
// bodies apart only by the identity of these handles.
type bodyHandle struct {
	io.ReadCloser

	once sync.Once
	err  error
}

// Close closes the body once.
func (b *bodyHandle) Close() error {
	b.once.Do(func() { b.err = b.ReadCloser.Close() })
	return b.err
}

// handleOf returns the bodyHandle body is (behind its write side), or nil.
func handleOf(body io.ReadCloser) *bodyHandle {
	h, _ := readSide(body).(*bodyHandle)
	return h
}

// isNoBody reports whether body is http.NoBody, by its type: no body value is
// ever compared.
func isNoBody(body io.ReadCloser) bool {
	return body != nil && reflect.TypeOf(body) == reflect.TypeOf(http.NoBody)
}

// own makes the attempt the owner of resp's body and returns the body to
// hand on in its place.
func (c *retryCall) own(resp *http.Response) io.ReadCloser {
	if resp.Body == nil {
		c.owned = nil
		return nil
	}

	h := &bodyHandle{ReadCloser: resp.Body}
	c.owned = h

	return withWriteSide(h, resp.Body)
}

// owns reports whether body is the handle of the body the last attempt
// received, still owned.
func (c *retryCall) owns(body io.ReadCloser) bool {
	return c.owned != nil && handleOf(body) == c.owned
}

// handOver settles the bodies when resp goes back to the caller: its body,
// whatever it is (the received one, a policy's replacement, an
// ErrorHandler's), passes to the caller; the owned and handed handles it is
// not are released when it is closed, or at once when there is none (nil or
// http.NoBody). The returned stream is never read or closed here.
func (c *retryCall) handOver(resp *http.Response, handles []*bodyHandle) {
	var returned io.ReadCloser
	if resp != nil {
		returned = resp.Body
	}

	h := handleOf(returned)

	var releases []io.Closer

	if c.owned != nil {
		if h != c.owned {
			releases = append(releases, c.owned)
		}

		c.owned = nil
	}

	for _, handed := range handles {
		if handed != nil && h != handed {
			releases = append(releases, handed)
		}
	}

	if len(releases) == 0 {
		return
	}

	if returned == nil || isNoBody(returned) {
		for _, r := range releases {
			_ = r.Close()
		}

		return
	}

	resp.Body = withWriteSide(&releasingBody{ReadCloser: returned, release: releases}, returned)
}

// releasingBody is a body handed to the caller in place of the received one:
// closing it also releases the handles it stands in for, once.
type releasingBody struct {
	io.ReadCloser

	release []io.Closer
	once    sync.Once
}

// Close closes the body, then releases the handles.
func (b *releasingBody) Close() error {
	err := b.ReadCloser.Close()

	b.once.Do(func() {
		for _, r := range b.release {
			_ = r.Close()
		}
	})

	return err
}

// dispose disposes of a discarded reply between attempts (see discardBody
// and releaseOwned).
func (c *retryCall) dispose(resp *http.Response) {
	if resp != nil {
		c.discardBody(resp.Body)
	}

	c.releaseOwned()
}

// discardBody disposes of a discarded body. The owned handle is drained first
// when the attempt ran under its own deadline and the reply as received was
// small enough: bodies of a larger known length and protocol upgrades are
// closed without reading, and so is every body of an attempt without a
// deadline, so a stalled upstream cannot block the caller. The drain stays
// synchronous although [http.Transport] drains an unread body after Close
// itself (Go 1.27): that drain runs in the background, so a retry sent right
// after Close finds the connection still busy and dials another. Any other
// body (a callback's) is closed directly, never read.
func (c *retryCall) discardBody(body io.ReadCloser) {
	if body == nil {
		return
	}

	if c.owns(body) {
		ob := c.owned
		c.owned = nil

		if c.attemptHasDeadline() && c.lastStatus != http.StatusSwitchingProtocols && c.lastContentLength <= maxDiscardDrain {
			_, _ = io.Copy(io.Discard, io.LimitReader(ob.ReadCloser, maxDiscardDrain))
		}

		_ = ob.Close()

		return
	}

	_ = body.Close()
}

// releaseOwned closes the owned handle, if still owned, without reading it.
func (c *retryCall) releaseOwned() {
	if c.owned != nil {
		_ = c.owned.Close()
		c.owned = nil
	}
}

// attemptHasDeadline reports whether the last attempt ran under a deadline,
// read from its private request, never from the view a policy may replace.
func (c *retryCall) attemptHasDeadline() bool {
	if c.lastAttemptReq == nil {
		return false
	}

	_, ok := c.lastAttemptReq.Context().Deadline()

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
