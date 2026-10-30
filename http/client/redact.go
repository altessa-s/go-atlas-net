// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/sony/gobreaker/v2"

	"github.com/altessa-s/go-atlas-core/errors/network"

	"github.com/altessa-s/go-atlas-net/http/client/limiters"
)

// redirectLimitRe matches the whole text of net/http's redirect limit error,
// "stopped after 10 redirects", and nothing that could carry a URL.
var redirectLimitRe = regexp.MustCompile(`\Astopped after \d+ redirects\z`)

// egressRuleLabelRe matches the rule labels an [EgressPolicy] builds
// (egressRuleLabel): a positional name or an [EgressRule] Name (validated to
// egressRuleNameRe's charset), then the normalized matchers; a host glob
// never holds ':', '/', '@' or whitespace, so a label of another shape (set
// by a caller) is not rendered.
var egressRuleLabelRe = regexp.MustCompile(`\A[A-Za-z0-9_.\-\[\]]{1,64}` +
	`(?: host=[a-z0-9._*?\[\]!^\\-]+)?(?: cidr=[0-9a-fA-F:.]+/\d{1,3})?(?: port=\d{1,5})?\z`)

// breakerNameRe matches a circuit breaker name of this package's shape (a
// configured identifier, optionally suffixed with a host); another shape (set
// by a caller) is not rendered.
var breakerNameRe = regexp.MustCompile(`\A[A-Za-z0-9 ._:\-\[\]]{1,128}\z`)

// redactedURL renders u for logs and errors without user info, query string
// and fragment, which may carry credentials or signed tokens.
func redactedURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath}).String()
}

// redactedRawURL is [redactedURL] for a caller-supplied URL string; an
// unparsable one is not quoted at all, as any part of it may be a secret.
func redactedRawURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparsable URL)"
	}
	return redactedURL(u)
}

// RedactError returns err without the request URL secrets of an error of the *[net/http.Client] that [New] returns, or of
// [net/http.NewRequestWithContext]: net/http wraps every failure in a fresh [*url.Error] that quotes the full request URL, and the
// text of its Err may quote a server-supplied URL too (an unparsable redirect Location, for example). It never modifies err, whose
// tree a transport may share between requests:
//
//   - a *url.Error is returned as a redacted copy: its URL loses the user info, query string and fragment, and its Err renders a
//     trusted message built as the retry layer builds its own (see the package documentation), the original Err as its cause;
//   - an error wrapping *url.Errors (fmt.Errorf with %w, errors.Join, a caller's type), whose own text may have captured a URL, is
//     returned inside an error whose text is the redacted *url.Errors' only, and through which [errors.As] finds the redacted copy
//     of the first of them;
//   - an error whose cause graph is too large to inspect fully ([maxRedactedErrors] visits) is replaced by an error of a fixed text
//     wrapping [ErrUninspectableCause]: its causes are dropped, as errors.Is and errors.As could not search them in bounded time.
//
// Otherwise the causes stay reachable, so [errors.Is], [errors.As], [url.Error.Timeout] and the Is helpers of this package and of
// core/errors (IsResourceRedirects, IsCertUnknownAuthority, IsUnsupportedProtocolScheme, ...) keep classifying the result. An error
// without a *url.Error is returned unchanged; nil stays nil. RedactError of its own result returns it unchanged.
//
// The [HTTPClient] wrappers and the [RequestBuilder] apply it already; call it on the errors of the plain *http.Client before
// logging or returning them.
//
// Example:
//
//	resp, err := client.Get(signedURL)
//	if err != nil {
//	    return httpclient.RedactError(err)
//	}
func RedactError(err error) error {
	switch e := err.(type) { //nolint:errorlint // only the outermost layer decides how err is returned
	case *transportError:
		return err // RedactError's own wrapper
	case *url.Error:
		if redactedURLError(e) {
			return err
		}
		return redactURLError(e)
	}
	if !errorGraphWithin(err) {
		// Too large to search for *url.Errors, and to keep as a cause: errors.Is and errors.As on the result would not return.
		return uninspectable()
	}
	urlErrs, _ := findURLErrors(err) // complete: findURLErrors visits a part of the graph errorGraphWithin bounded
	if len(urlErrs) == 0 {
		return err
	}
	texts := make([]string, len(urlErrs))
	for i, ue := range urlErrs {
		urlErrs[i] = redactURLError(ue)
		texts[i] = urlErrs[i].Error()
	}
	return &transportError{cause: err, msg: strings.Join(texts, "\n"), urlErr: urlErrs[0]}
}

// redactURLError returns a redacted copy of ue, the original Err as the cause of the copy's.
func redactURLError(ue *url.Error) *url.Error {
	return &url.Error{Op: safeOp(ue.Op), URL: redactedRawURL(ue.URL), Err: redactTransportError(ue.Err)}
}

// redactedURLError reports whether ue is already redacted, as [redactURLError] returns it.
func redactedURLError(ue *url.Error) bool {
	_, trusted := ue.Err.(*transportError) //nolint:errorlint // the copy's own Err
	return trusted && ue.URL == redactedRawURL(ue.URL) && ue.Op == safeOp(ue.Op)
}

// maxRedactedErrors bounds the errors [findURLErrors] visits and holds pending in one tree, which a caller's Unwrap may make
// cyclic or huge.
const maxRedactedErrors = 1024

// findURLErrors returns the *url.Errors in the tree of err, in depth-first order, without descending into them (their Err is
// rendered as trusted text). complete is false when the tree holds more than [maxRedactedErrors] errors to visit or to hold
// pending, so some error may not have been inspected.
func findURLErrors(err error) (urlErrs []*url.Error, complete bool) {
	stack := []error{err}
	for visited := 0; len(stack) > 0; visited++ {
		if visited >= maxRedactedErrors {
			return nil, false
		}
		e := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch u := e.(type) { //nolint:errorlint // the tree is walked one layer at a time
		case nil:
		case *url.Error:
			urlErrs = append(urlErrs, u)
		case interface{ Unwrap() []error }:
			children := u.Unwrap()
			if len(stack)+len(children) > maxRedactedErrors {
				return nil, false
			}
			for _, child := range slices.Backward(children) { // pushed last to first, so the first child is visited first
				stack = append(stack, child)
			}
		case interface{ Unwrap() error }:
			stack = append(stack, u.Unwrap())
		}
	}
	return urlErrs, true
}

// transportError is a transport failure as the retry layer reports it: its
// message is rendered from trusted parts of the cause only, because the text
// of an arbitrary transport error may embed the request URL with its query
// string (a [*url.Error], a wrapper formatting req.URL, a custom dialer). The
// cause stays reachable through Unwrap, so [errors.Is] and [errors.As] keep
// classifying it.
type transportError struct {
	cause  error
	msg    string
	urlErr *url.Error // the redacted copy errors.As finds first, for a wrapper RedactError returns
}

// redactTransportError wraps a transport failure into a [transportError]; nil
// and an already redacted error are returned as they are. A failure whose
// cause graph is too large to search ([errorGraphWithin]) — shared causes
// repeated exponentially, a cause unwrapping to itself — is replaced by
// [ErrUninspectableCause]: the retry layer and its callers classify the
// result with errors.Is and errors.As, which would not return on it.
func redactTransportError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*transportError); ok { //nolint:errorlint // only the outermost layer is checked
		return err
	}
	if !errorGraphWithin(err) {
		return uninspectable()
	}
	return &transportError{cause: err, msg: renderTrusted(err)}
}

// ErrUninspectableCause replaces the cause of an error whose cause graph is too large to search in bounded time (shared causes
// repeated exponentially, a cause unwrapping to itself): [RedactError] and the client return an error of a fixed text wrapping it,
// so that errors.Is and errors.As on what they return always return. Every [transportError] holds a cause graph within
// [maxRedactedErrors] visits, or this.
var ErrUninspectableCause = errors.New("httpclient: error cause graph too large to inspect")

// uninspectable returns the error standing for a failure whose cause graph is too large to inspect.
func uninspectable() error {
	return &transportError{cause: ErrUninspectableCause, msg: "HTTP transport error (redacted)"}
}

// Error returns the trusted message of the cause.
func (e *transportError) Error() string { return e.msg }

// Unwrap returns the original transport error.
func (e *transportError) Unwrap() error { return e.cause }

// As sets a *url.Error target to the redacted copy of the first *url.Error of a wrapper [RedactError] returned, before
// [errors.As] reaches the original in the cause.
func (e *transportError) As(target any) bool {
	if p, ok := target.(**url.Error); ok && e.urlErr != nil {
		*p = e.urlErr
		return true
	}
	return false
}

// Timeout reports whether the cause is a timeout; [url.Error.Timeout] asks
// its Err directly, without unwrapping.
func (e *transportError) Timeout() bool {
	if !errorGraphWithin(e.cause) {
		return false
	}
	t, ok := errors.AsType[interface {
		error
		Timeout() bool
	}](e.cause)
	return ok && t.Timeout()
}

// Temporary reports whether the cause is temporary; [url.Error.Temporary]
// asks its Err directly, without unwrapping.
func (e *transportError) Temporary() bool {
	if !errorGraphWithin(e.cause) {
		return false
	}
	t, ok := errors.AsType[interface {
		error
		Temporary() bool
	}](e.cause)
	return ok && t.Temporary()
}

// trustedMessage describes err as [renderTrusted] does, once its cause graph is known to be small: renderTrusted searches it with
// errors.As and errors.Is many times over, which a graph of shared or repeated causes (errors.Join(e, e) nested forty times) makes
// exponential, and a cause unwrapping to itself endless. A graph larger than [maxRedactedErrors] visits gets a fixed text.
func trustedMessage(err error) string {
	if !errorGraphWithin(err) {
		return "HTTP transport error (redacted)"
	}
	return renderTrusted(err)
}

// errorGraphWithin reports whether walking the cause graph of err (Unwrap() error and Unwrap() []error), visiting a shared cause
// once per path to it as errors.Is and errors.As do, takes at most [maxRedactedErrors] visits.
func errorGraphWithin(err error) bool {
	stack := []error{err}
	for visited := 0; len(stack) > 0; visited++ {
		if visited >= maxRedactedErrors {
			return false
		}
		e := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch u := e.(type) { //nolint:errorlint // the graph is walked one layer at a time
		case interface{ Unwrap() []error }:
			children := u.Unwrap()
			if len(stack)+len(children) > maxRedactedErrors {
				return false
			}
			stack = append(stack, children...)
		case interface{ Unwrap() error }:
			if next := u.Unwrap(); next != nil {
				stack = append(stack, next)
			}
		}
	}
	return true
}

// renderTrusted describes err from fields that cannot hold a URL query:
// the messages of this package's own error types, fixed sentinel texts,
// host names, socket addresses and system error numbers. Anything else is
// reported by its kind only. The renderers below are tried in order.
func renderTrusted(err error) string {
	for _, render := range [...]func(error) (string, bool){
		renderRetryWrapper, renderOwnError, renderSentinel, renderNetError, renderURLError,
	} {
		if msg, ok := render(err); ok {
			return msg
		}
	}
	return fmt.Sprintf("HTTP transport error (%T)", rootCause(err))
}

// renderRetryWrapper renders the wrappers of the retry layer, outermost
// only: their own text is built from trusted parts and a nested error,
// rendered by renderTrusted in turn.
func renderRetryWrapper(err error) (string, bool) {
	switch e := err.(type) { //nolint:errorlint // only the outermost layer is checked
	case nil:
		return "<nil>", true
	case *transportError:
		return e.msg, true
	case *RetryExhaustedError:
		return fmt.Sprintf("failed after %d attempts for %s %s: %s",
			e.Attempts, safeMethod(e.Method), redactedRawURL(e.URL), trustedMessage(e.LastError)), true
	case *NonRetryableError:
		return "non-retryable error: " + trustedMessage(e.Err), true
	case *retryableStatusError:
		return e.Error(), true
	}
	return "", false
}

// renderOwnError renders this package's and its limiters' error types found
// anywhere in err's chain.
func renderOwnError(err error) (string, bool) {
	// A transport error redacted below a wrapper (the circuit breaker's) already carries its trusted text.
	if e, ok := errors.AsType[*transportError](err); ok {
		return e.msg, true
	}
	if e, ok := errors.AsType[*SSRFError](err); ok {
		rule := e.Rule
		if rule != "" && !egressRuleLabelRe.MatchString(rule) {
			rule = "(unlabeled rule)"
		}
		return (&SSRFError{Host: hostOnly(e.Host), IP: hostOnly(e.IP), Rule: rule}).Error(), true
	}
	if e, ok := errors.AsType[*ResponseSizeError](err); ok {
		return e.Error(), true
	}
	if e, ok := errors.AsType[*CircuitBreakerError](err); ok {
		return breakerMessage(e), true
	}
	if e, ok := errors.AsType[*UnexpectedStatusError](err); ok {
		return statusMessage(*e), true
	}
	if e, ok := errors.AsType[UnexpectedStatusError](err); ok {
		return statusMessage(e), true
	}
	if e, ok := errors.AsType[*limiters.RateLimitError](err); ok {
		return rateLimitMessage(e), true
	}
	return "", false
}

// breakerMessage renders a circuit breaker error from a well-formed breaker
// name and a known state only.
func breakerMessage(e *CircuitBreakerError) string {
	name := e.Name
	if !breakerNameRe.MatchString(name) {
		name = "(unnamed)"
	}
	state := e.State
	if state != "open" && state != "too_many_requests" && state != "half-open" {
		state = "unavailable"
	}
	return (&CircuitBreakerError{Name: name, State: state}).Error()
}

// rateLimitMessage renders a rate limit error from its host, delay and
// trusted cause.
func rateLimitMessage(e *limiters.RateLimitError) string {
	msg := fmt.Sprintf("rate limit exceeded for host %q", hostOnly(e.Host))
	if e.RetryAfter > 0 {
		msg += fmt.Sprintf(", retry after %v", e.RetryAfter)
	}
	if e.Err != nil {
		msg += ": " + trustedMessage(e.Err)
	}
	return msg
}

// renderSentinel renders the fixed text of a known sentinel in err's chain.
func renderSentinel(err error) (string, bool) {
	for _, sentinel := range []error{
		ErrSSRFBlocked, limiters.ErrRateLimited, gobreaker.ErrOpenState, gobreaker.ErrTooManyRequests,
		context.Canceled, context.DeadlineExceeded,
	} {
		if errors.Is(err, sentinel) {
			return sentinel.Error(), true
		}
	}
	return "", false
}

// renderNetError renders network, TLS, system and net/http failures from
// their trusted parts.
func renderNetError(err error) (string, bool) {
	if e, ok := errors.AsType[*net.DNSError](err); ok {
		return dnsMessage(e), true
	}
	if msg, ok := tlsMessage(err); ok {
		return msg, true
	}
	if e, ok := errors.AsType[*net.OpError](err); ok {
		return opMessage(e), true
	}
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		return errno.Error(), true
	}
	if e, ok := errors.AsType[net.Error](err); ok && e.Timeout() {
		return "i/o timeout", true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return io.ErrUnexpectedEOF.Error(), true
	}
	if errors.Is(err, io.EOF) {
		return io.EOF.Error(), true
	}
	if unsupportedScheme(err) {
		return "unsupported protocol scheme", true
	}
	// net/http's redirect limit: a fixed text naming a count only, which
	// core/errors.IsResourceRedirects matches.
	if msg := rootCause(err).Error(); redirectLimitRe.MatchString(msg) {
		return msg, true
	}
	return "", false
}

// renderURLError renders a URL parse failure, which names a single escape
// sequence or character. net/url puts at most a 3-byte escape or one
// character into these; a longer value was built by someone else and is not
// quoted.
func renderURLError(err error) (string, bool) {
	if e, ok := errors.AsType[url.EscapeError](err); ok {
		const maxEscape = len("%zz") // net/url quotes one percent-escape at most
		if len(e) <= maxEscape {
			return e.Error(), true
		}
		return "invalid URL escape", true
	}
	if e, ok := errors.AsType[url.InvalidHostError](err); ok {
		if utf8.RuneCountInString(string(e)) <= 1 {
			return e.Error(), true
		}
		return "invalid character in host name", true
	}
	return "", false
}

// statusMessage renders a status error from its status and request fields
// only: the response Body is free-form upstream text that may echo a request
// URL, and a URI or Host set by a caller (a policy or error handler) may carry
// a query string, fragment or user info, which are cut off.
func statusMessage(e UnexpectedStatusError) string {
	return fmt.Sprintf("unexpected response status: %d (%s); request: %s %s%s",
		e.Status, http.StatusText(e.Status), cutQuery(e.Method), hostOnly(e.Host), cutQuery(e.URI))
}

// cutQuery drops everything from the first '?' or '#' of s, where a query
// string or fragment would start.
func cutQuery(s string) string {
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		return s[:i]
	}
	return s
}

// hostOnly reduces a host-like field set by a caller to the host itself: user
// info is dropped and anything from the first '/', '?' or '#' is cut off, so a
// whole URL put into the field cannot carry its path, query or fragment along.
func hostOnly(s string) string {
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// dnsMessage renders a resolver failure as net does, without its free-form
// Err text.
func dnsMessage(e *net.DNSError) string {
	reason := "lookup failed"
	switch {
	case e.IsNotFound:
		reason = "no such host"
	case e.IsTimeout:
		reason = "i/o timeout"
	}
	return "lookup " + hostOnly(e.Name) + ": " + reason
}

// safeTokenRe matches a short identifier such as a socket operation ("dial",
// "read") or network ("tcp4"), which cannot carry a URL.
var safeTokenRe = regexp.MustCompile(`\A[a-z0-9]{1,16}\z`)

// methodRe matches an HTTP method token.
var methodRe = regexp.MustCompile(`\A[A-Za-z]{1,16}\z`)

// safeMethod returns m when it is an HTTP method token, otherwise a placeholder.
func safeMethod(m string) string {
	if methodRe.MatchString(m) {
		return m
	}
	return "?"
}

// urlOpRe matches the Op of a *url.Error as net/http and net/url set it: a method in title case ("Get", "Post") or "parse".
var urlOpRe = regexp.MustCompile(`\A[A-Za-z]{1,16}\z`)

// safeOp returns the Op of a *url.Error when it has the shape net/http gives it, otherwise a placeholder.
func safeOp(op string) string {
	if urlOpRe.MatchString(op) {
		return op
	}
	return "?"
}

// safeToken returns s when it is a short identifier, otherwise a placeholder.
func safeToken(s string) string {
	if safeTokenRe.MatchString(s) {
		return s
	}
	return "?"
}

// tlsMessage renders certificate and TLS protocol failures from fixed texts,
// the certificate-derived text of an unknown authority, alert codes and a
// host reduced by [hostOnly]: the free-form fields of these types (a hostname
// error's Host, an invalid certificate's Detail, a record header error's Msg)
// may be set by a custom transport and are not rendered as they are.
func tlsMessage(err error) (string, bool) {
	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
		// Fixed text: Error() may append a CommonName chosen by the server.
		return "x509: certificate signed by unknown authority", true
	}
	if e, ok := errors.AsType[x509.HostnameError](err); ok {
		return "x509: certificate is not valid for " + hostOnly(e.Host), true
	}
	if _, ok := errors.AsType[x509.CertificateInvalidError](err); ok {
		return "x509: certificate is not valid", true
	}
	if e, ok := errors.AsType[tls.AlertError](err); ok {
		return e.Error(), true
	}
	if _, ok := errors.AsType[tls.RecordHeaderError](err); ok {
		return "tls: invalid record header", true
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return "tls: failed to verify certificate", true
	}
	return "", false
}

// opMessage renders a socket failure as "op net addr: reason", taking the
// reason from a system error number or a timeout only. Op and Net must be
// short identifiers and the address is reduced by [hostOnly], since a custom
// transport may fill these fields with anything.
func opMessage(e *net.OpError) string {
	msg := safeToken(e.Op)
	if e.Net != "" {
		msg += " " + safeToken(e.Net)
	}
	if e.Addr != nil {
		msg += " " + hostOnly(e.Addr.String())
	}
	switch errno, ok := errors.AsType[syscall.Errno](e.Err); {
	case ok:
		return msg + ": " + errno.Error()
	case e.Timeout():
		return msg + ": i/o timeout"
	default:
		return msg + ": failed"
	}
}

// unsupportedScheme reports the error net/http's Transport returns for a URL
// scheme it does not support. Its type is unexported and core/errors
// recognizes it inside a [*url.Error] only, which a round tripper never
// sees, so the text of the root cause is matched.
func unsupportedScheme(err error) bool {
	return network.IsUnsupportedProtocolScheme(&url.Error{Err: rootCause(err)})
}

// rootCause follows the single-error Unwrap chain of err to its end.
func rootCause(err error) error {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err
		}
		err = next
	}
}
