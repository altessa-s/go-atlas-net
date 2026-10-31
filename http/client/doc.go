// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

// Package client provides a resilient HTTP client with automatic retries, circuit breaker protection,
// rate limiting, and SSRF safeguards. It wraps the standard [net/http.Client] with additional
// features for high-availability systems.
//
// Two constructors are available:
//
//   - [New] returns a bare *[net/http.Client] suitable for use with any code that
//     expects the standard type.
//   - [NewHTTPClient] returns an [HTTPClient] with convenience methods (Get, PostJSON,
//     fluent [RequestBuilder], etc.) layered on top.
//
// [HTTPClient] composes small interfaces — [httpx.Doer] (Do), [VerbClient] (Get, Post, PostJSON, ...),
// [OptionsClient] (the *WithOptions methods) and [RequestFactory] (NewRequest) — so consumers accept the
// narrowest one they need, usually an [httpx.Doer].
//
// # Features
//
//   - Connection Management: automatic TCP connection pooling and keep-alive configuration; [DefaultPooledTransport]
//     returns a fresh copy of the default transport to customize and pass to [WithTransport].
//   - Resilience: built-in retry policies with exponential backoff and jitter ([WithRetryMax], [WithRetryWait]);
//     [WithoutRetry] sends each request once and returns every reply unclassified. Retries after a 429/503
//     reply wait at least its Retry-After ([WithRetryAfterMax], [ParseRetryAfter]). The built-in classification retries
//     [DefaultRetryableStatuses] (change them with [WithRetryableStatuses]) and is exported as [DefaultRetryPolicy] for
//     custom policies set with [WithRetryPolicyHandler]. The built-in classification replays only requests that are
//     safe to repeat, as net/http does: an idempotent method (GET, HEAD, OPTIONS, TRACE, PUT, DELETE) or a request
//     with an Idempotency-Key or X-Idempotency-Key header; a POST or PATCH without one is sent once. A policy set
//     with [WithRetryPolicyHandler] decides for every method. [WithClock] sets the time source of the retry layer (a clock.Fake in tests).
//   - Protection: a circuit breaker per destination host, so one failing endpoint never rejects requests to
//     another; tune hosts with [WithCircuitBreakerSettings] and bound lazily created breakers with [WithMaxHostBreakers].
//   - Redirects: followed like net/http by default; [WithoutRedirects] returns a 3xx reply to the caller as the
//     response instead, on every layer of the client.
//   - Rate Limiting: pluggable client-side rate limiting via [WithLimiter] and [limiters.RequestsLimiter].
//   - SSRF Protection: default blocking of connections to private/local IPs; see [WithSSRFProtection] and [WithSSRFAllowedCIDRs].
//     [SSRFDialControl] exports the dial guard for services that build their own transport.
//   - Egress Policy: allow/deny rules matching CIDR, host glob and port ([EgressRule], [NewEgressPolicy]); deny
//     wins and allow rules exempt private ranges. Install with [WithEgressPolicy], in a caller transport with
//     [EgressPolicy.DialContext], or for proxied dials as proxydial.WithSSRFGuard(policy.Check).
//   - Proxy: declarative outbound proxy configuration via [WithProxy], [WithProxyURL], [WithProxyFunc],
//     or [WithoutProxy]; environment proxies apply only with [WithoutSSRFProtection].
//   - Observability: request logging through [WithLogger], structured error types for inspection,
//     and telemetry events delivered to an [Observer] set with [WithObserver] (see Observer below).
//
// # Error Handling
//
// All errors implement [errors.Is] matching against package-level sentinels.
// Use the typed extraction helpers for detailed inspection:
//
//   - [IsUnexpectedStatusError] extracts [UnexpectedStatusError] (matches [ErrUnexpectedStatus]).
//   - [IsCircuitBreakerError] / [IsCircuitBreakerOpen] inspect [CircuitBreakerError] (matches [ErrCircuitBreakerOpen]).
//   - [IsResponseSizeError] extracts [ResponseSizeError] (matches [ErrResponseSizeExceeded]); it keeps the status and
//     header of the oversized reply, whose body is closed unread, and is never retried.
//   - limiters.IsRateLimitError extracts limiters.RateLimitError (matches
//     limiters.ErrRateLimited), returned for a [WithLimiter] denial.
//   - [IsRetryExhaustedError] extracts [RetryExhaustedError] (matches [ErrMaxRetriesExceeded]).
//   - [IsSSRFError] extracts [SSRFError] (matches [ErrSSRFBlocked]).
//   - [IsTemporaryError] checks for transient conditions that may succeed on retry.
//
// # Redaction
//
// The client keeps the query string and user info of a request URL, which may carry credentials, out of its own
// errors and logs: [UnexpectedStatusError.URI] is the escaped path, [RetryExhaustedError.URL] and the retry log
// show scheme, host and path only. A transport error leaves the retry layer with a message rendered from trusted
// parts of the cause only (this package's error types, sentinel texts, host names, socket addresses, system error
// numbers, TLS errors, net/http's redirect limit); the cause itself stays reachable through [errors.Is] and
// [errors.As]. So does an error a [RetryPolicyHandler] or an [ErrorHandler] returns, and a failure to buffer or
// rewind the request body.
//
// net/http wraps every failure of [net/http.Client.Do] in a [net/url.Error] that quotes the full request URL, and its
// nested text may quote a server-supplied URL (an unparsable redirect Location). The [HTTPClient] methods and
// [RequestBuilder.Send] pass their errors through [RedactError], which strips the URL and renders the nested text
// from trusted parts; a caller of the *http.Client that [New] returns calls RedactError on its errors itself.
//
// # Observer
//
// The client records no metrics and reports no health itself: it delivers its telemetry events to the
// [Observer] set with [WithObserver] (default [NoopObserver]). [New] calls [Observer.Attach] once with a
// read-only [Breakers] view of the circuit breakers, then reports every completed request
// ([Observer.RequestDone], with its status class, retry count, failure and duration), every retry, every
// attempt rejected by a breaker and every breaker state transition. [Observer.BreakerStateChanged] runs with
// the breaker's lock held: it must not read [Breakers] synchronously.
//
// Embed [NoopObserver] to implement only the events of interest:
//
//	c := client.New(client.WithObserver(myObserver))
//
// # Usage
//
//	c := client.New(
//	    client.WithRetryMax(3),
//	    client.WithRetryWait(1*time.Second, 5*time.Second),
//	)
//
//	resp, _ := c.Get("https://api.example.com/v1/data")
//	defer resp.Body.Close()
//
// SSRF protection also blocks loopback and disables environment proxies. An
// explicit proxy option is allowed: the dial to the proxy is guarded (allow its
// network with [WithSSRFAllowedCIDRs]), the proxy resolves the target host, and
// a target given as a literal private IP is refused before reaching the proxy.
// A proxy TLS configuration, custom TLS dialers, a proxy resolver set on a
// caller transport and non-standard transports require [WithoutSSRFProtection];
// otherwise construction panics with [ErrSSRFUnsafeTransport].
//
// # Egress policy
//
// An [EgressPolicy] replaces the built-in private-address check with rules
// compiled once by [NewEgressPolicy]. A destination is refused when a deny rule
// matches (deny wins), allowed when an allow rule matches, and otherwise refused
// only when its resolved address is private, loopback or link-local. The check
// runs in the dial control hook on the address actually connected to, so DNS
// rebinding cannot swap it. Every refusal is an [*SSRFError] matching
// [ErrSSRFBlocked]; for a deny rule its Rule field names the rule.
//
//	policy, err := client.NewEgressPolicy(
//	    []client.EgressRule{{CIDR: netip.MustParsePrefix("10.20.0.0/16"), Host: "*.corp.example"}},
//	    []client.EgressRule{{Host: "*.internal"}, {Port: 25}},
//	)
//	if err != nil {
//	    return err
//	}
//	c := client.New(client.WithEgressPolicy(policy))
package client
