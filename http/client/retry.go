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

// attemptRequest prepares req, an attempt's private request, for sending:
// with a cookie jar, its Cookie header becomes the caller's original one
// (callerCookie, captured before any attempt) plus the jar's cookies for the
// URL read now, edited in place; with an attempt timeout, it returns a copy
// under its own deadline and the deadline's cancel func.
func (rt *retryRoundTripper) attemptRequest(
	ctx context.Context, req *http.Request, callerCookie []string,
) (*http.Request, context.CancelFunc) {
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

// receivedCookieNames returns the names of the cookies resp sets.
func receivedCookieNames(resp *http.Response) []string {
	cookies := resp.Cookies()
	names := make([]string, 0, len(cookies))

	for _, c := range cookies {
		names = append(names, c.Name)
	}

	return names
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
			resp, err := rt.finish(nil, nil, bufErr, 0)
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
		retryErr = exhausted(call.base, call.lastResp, retryErr, call.attempts)
	}
	// A pass-through policy (WithoutRetry) clears retryErr but leaves the
	// attempt's error in lastErr, which finish returns: still a failure.
	failed := retryErr != nil || call.lastErr != nil
	resp, err := rt.finish(call.lastResp, call.lastErr, retryErr, call.attempts)
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
	if req.Body == nil || req.Body == http.NoBody || req.GetBody != nil || rt.maxAttempts == 0 {
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
func (c *retryCall) captureTrailersAtEOF(body io.ReadCloser) *eofHookBody {
	hooked := &eofHookBody{ReadCloser: body, onEOF: c.captureLiveTrailers, closed: make(chan struct{})}

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
// caller's live trailers is closed: until then the transport may still read
// one and fill those trailers, which the next attempt must not share, and the
// caller's GetBody must not be asked for another body.
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
func (c *retryCall) responseView(req *http.Request, resp *http.Response) *http.Request {
	ctx := req.Context()
	if c.rt.jar != nil {
		ctx = context.WithValue(ctx, receivedCookiesKey{}, receivedCookieNames(resp))
	}

	view := req.WithContext(ctx)
	view.Body = nil
	view.GetBody = nil
	view.Header = req.Header.Clone()
	view.TransferEncoding = slices.Clone(req.TransferEncoding)
	// The redirect history belongs to http.Client, whose redirect handling
	// reads it: the view does not expose it.
	view.Response = nil

	if final, ok := c.finalTrailers(); ok {
		view.Trailer = final.Clone()
	} else {
		view.Trailer = c.base.Trailer.Clone()
	}

	if req.URL != nil {
		u := *req.URL // Userinfo is immutable: sharing it is safe.
		view.URL = &u
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
type eofHookBody struct {
	io.ReadCloser
	onEOF     func()
	done      bool
	closed    chan struct{}
	closeOnce sync.Once
}

// Close closes the body and signals closed.
func (b *eofHookBody) Close() error {
	err := b.ReadCloser.Close()
	b.closeOnce.Do(func() { close(b.closed) })

	return err
}

// Read reads from the body and calls onEOF on its end.
func (b *eofHookBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF && !b.done { //nolint:errorlint // io.Reader returns io.EOF itself, never wrapped
		b.done = true
		b.onEOF()
	}

	return n, err
}

// prepareTrailers sets the trailers of attempt: a copy of the final ones when
// known; otherwise the caller's live map, with the attempt's body (and any
// body the transport rewinds) hooked to capture them at its end.
func (c *retryCall) prepareTrailers(attempt *http.Request) {
	if final, ok := c.finalTrailers(); ok {
		attempt.Trailer = final.Clone()
	} else {
		// No transmission has read the body to its end yet: this one sends
		// the caller's live map, which the (rewound) body fills as it is
		// read, and captures its values when the body reaches EOF.
		attempt.Trailer = c.req.Trailer
		if c.req.Trailer != nil && attempt.Body != nil && attempt.Body != http.NoBody {
			attempt.Body = c.captureTrailersAtEOF(attempt.Body)
			// A body net/http rewinds itself is hooked the same way.
			if c.getBody != nil {
				attempt.GetBody = func() (io.ReadCloser, error) {
					body, err := c.getBody()
					if err != nil || body == nil || body == http.NoBody {
						return body, err
					}

					return c.captureTrailersAtEOF(body), nil
				}
			}
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
		closeBody(c.lastResp)
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
	closeBody(c.lastResp)
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

	hasBody := c.firstBody != nil && c.firstBody != http.NoBody
	if !c.firstAttempt && hasBody {
		// No body may still write the live trailers when the caller's
		// GetBody is asked for the next one.
		if err := c.awaitLiveBodies(ctx); err != nil {
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

	c.prepareTrailers(attempt)

	c.firstAttempt = false

	attemptReq, cancel := c.rt.attemptRequest(ctx, attempt, c.callerCookie)
	c.attempts++
	c.bodyHanded = true
	resp, err := c.rt.next.RoundTrip(attemptReq) //nolint:bodyclose // resp is stored in lastResp and either closed on next retry or returned to caller
	// From here on a transport error renders trusted text only: its own
	// text may embed the request URL with its query string.
	err = redactTransportError(err)
	resp = releaseWithBody(resp, err, cancel) //nolint:bodyclose // same response, closed like resp above
	// Every reply's cookies are stored exactly once, on receipt, whatever
	// happens to the reply next (retried, returned, discarded).
	c.rt.storeCookies(c.base, resp)

	if resp != nil {
		// Policies, the ErrorHandler and the caller see a detached view of
		// the attempt: editing it reaches neither the request the transport
		// may still be sending (an early reply) nor the next attempt.
		resp.Request = c.responseView(attemptReq, resp)
	}

	c.lastResp = resp
	c.lastErr = err

	if resp != nil {
		c.lastStatus = resp.StatusCode
	}

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
