# client

```go
import "github.com/altessa-s/go-atlas-net/http/client"
```

Package `client` provides a resilient HTTP client with automatic retries, circuit breaker protection, rate limiting, and SSRF safeguards. Two
constructors are available: `New` returns a bare `*http.Client` suitable for any code expecting the standard type, and `NewHTTPClient` returns an
`HTTPClient` with convenience methods (Get, PostJSON, fluent RequestBuilder) layered on top.

## Features

| Feature            | Description                                                                                                            |
|--------------------|------------------------------------------------------------------------------------------------------------------------|
| Connection pooling | Automatic TCP connection pooling and keep-alive configuration via pooled transport                                     |
| Retry              | Exponential backoff with jitter, configurable min/max wait and attempt count                                           |
| Circuit breaker    | A breaker per destination host (bounded by `WithMaxHostBreakers`), with per-host overrides and state-change callbacks  |
| Rate limiting      | Pluggable client-side rate limiting via `WithLimiter` and `limiters.RequestsLimiter`                                   |
| SSRF protection    | Blocks connections to private/local IPs after DNS resolution, with CIDR allowlist                                      |
| Proxy              | Declarative HTTP/SOCKS5 proxy via `WithProxy`/`WithProxyURL`/`WithProxyFunc`, or `WithoutProxy` to bypass env defaults |
| Structured errors  | Typed errors with `errors.Is` matching and extraction helpers for each failure mode                                    |

## Options

| Option                       | Default       | Description                                                                                                                                          |
|------------------------------|---------------|------------------------------------------------------------------------------------------------------------------------------------------------------|
| `WithRetryMax`               | 5             | Maximum number of retry attempts                                                                                                                     |
| `WithRetryWait`              | 500ms/10s     | Minimum and maximum wait between retries (exponential backoff)                                                                                       |
| `WithRetryWaitMin`           | 500ms         | Minimum wait between retries                                                                                                                         |
| `WithRetryWaitMax`           | 10s           | Maximum wait between retries                                                                                                                         |
| `WithRetryFactor`            | 1.5           | Exponential growth of the retry backoff per attempt                                                                                                  |
| `WithRetryJitter`            | 0.25          | Fraction (0–1) of each backoff added as random jitter; 0 = deterministic                                                                             |
| `WithRetryMaxElapsed`        | 0             | Wall-clock bound of all attempts of one request; a retry that would end past it is not started (0 = none)                                           |
| `WithAttemptTimeout`         | 0             | Bound each attempt (limiter wait under `WithLimiterPerAttempt`, connection, reply, body) instead of the whole request; retries and Retry-After sleeps are not cut short |
| `WithRetryableStatuses`      | transient     | Reply statuses the built-in classification retries (`DefaultRetryableStatuses()`: 408, 429, 502, 503, 504); replaces the set, an empty call keeps it |
| `WithRetryAfterMax`          | 1m            | Cap on an honored `Retry-After` of a 429/503 reply: the retry waits at least that long, beyond `WithRetryWaitMax`; 0 ignores `Retry-After`            |
| `WithBreakerMaxRequests`     | 3             | Requests allowed in half-open state                                                                                                                  |
| `WithBreakerInterval`        | 30s           | Period to clear failure counts in closed state                                                                                                       |
| `WithBreakerTimeout`         | 30s           | Duration before transitioning from open to half-open                                                                                                 |
| `WithBreakerName`            | auto          | Circuit breaker name for identification                                                                                                              |
| `WithCircuitBreakerSettings` | --            | Per-host circuit breaker configuration                                                                                                               |
| `WithMaxHostBreakers`        | 1024          | Cap on breakers created lazily for hosts without explicit settings; further hosts share one breaker (0 = all such hosts share it)                   |
| `WithMaxResponseSize`        | 0             | Maximum response body size in bytes (0 = unlimited)                                                                                                  |
| `WithLimiter`                | nil           | Pluggable client-side rate limiter                                                                                                                   |
| `WithLimiterPerAttempt`      | off           | Place the limiter under the retry layer so every retry attempt passes it; a refusal is never retried                                               |
| `WithSSRFProtection`         | true          | Enable blocking of connections to private/local IP addresses                                                                                         |
| `WithoutSSRFProtection`      | --            | Disable SSRF protection; required for explicit proxies, custom TLS dialers, and non-`*http.Transport` round trippers                                 |
| `WithSSRFAllowedCIDRs`       | --            | CIDR prefixes exempted from SSRF blocking                                                                                                            |
| `WithEgressPolicy`           | nil           | Allow/deny `EgressPolicy` (CIDR, host glob, port; deny wins) in place of the built-in private-address check; off with `WithoutSSRFProtection`       |
| `WithLogger`                 | discard       | Structured logger for request logging                                                                                                                |
| `WithClient`                 | pooled        | Custom `*http.Client` base                                                                                                                           |
| `WithCookieJar`              | nil           | Cookie jar owned by the retry layer (per attempt and per redirect hop, each reply stored once); a `WithClient` jar is adopted the same way |
| `WithTransport`              | --            | Custom `*http.Transport` override; start from `DefaultPooledTransport()` (a fresh clone of the default) to keep the pool tuning                      |
| `WithProxy`                  | none          | Route requests through `http://host:port` with optional `*url.Userinfo`                                                                              |
| `WithProxyURL`               | none          | Route requests through any proxy URL (http, https, socks5, socks5h)                                                                                  |
| `WithProxyFunc`              | none          | Custom resolver matching `http.Transport.Proxy` signature                                                                                            |
| `WithoutProxy`               | none          | Disable proxy resolution, including `HTTP_PROXY`/`HTTPS_PROXY` defaults                                                                              |
| `WithProxyTLSConfig`         | system        | Custom `*tls.Config` for the handshake to an `https://` proxy (self-signed CA, mTLS); isolates proxy TLS from destination TLS via custom DialContext |
| `WithErrorHandler`           | nil           | Custom error handler                                                                                                                                 |
| `WithRetryPolicyHandler`     | nil           | Custom retry policy handler; delegate to `DefaultRetryPolicy` for the cases it does not decide                                                      |
| `WithoutRetry`               | --            | One attempt per request; the response for any status (body unread) or the transport error (redacted text, causes intact) is returned; overrides `WithRetryMax` and `WithRetryPolicyHandler` |
| `WithoutRedirects`           | follow        | Never follow redirects; a 3xx reply is returned as the response (also over a `WithClient` client, left unchanged) |
| `WithClock`                  | system        | `clock.Clock` of the retry layer: elapsed budget, backoff and Retry-After waits, Retry-After dates, request duration; `clock.NewFake` in tests |
| `WithObserver`               | no-op         | `Observer` receiving request, retry and circuit breaker events, for example to record metrics and health                                            |

With SSRF protection on (the default) environment proxies are ignored (`HTTP_PROXY`/`HTTPS_PROXY` apply only after `WithoutSSRFProtection()`);
explicit proxy options are allowed and guarded, see [SSRF and proxy policy](#ssrf-and-proxy-policy).

## Cookies

With a cookie jar — `WithCookieJar(jar)`, or the `Jar` of a `WithClient` client — the retry layer owns cookie handling and the returned
`*http.Client` has no `Jar`. Every attempt and every redirect hop sends the caller's own `Cookie` header plus the jar's cookies for its URL, read at
attempt time, and every reply's cookies are stored exactly once, when it is received — retried, failed and redirect replies included (a 503 challenge
cookie reaches the retry and the next request). A cookie another request changed, deleted or let expire meanwhile is sent in its current state, and a
retry cut short never re-stores an old reply over a newer value. The rest mirrors `http.Client`'s own jar handling: cookies are looked up and stored
under `req.Host` when it is set (a virtual host), and on a redirect chain an initial cookie that a redirect reply replaced or deleted is no longer
forwarded (the caller's other cookies are, rebuilt sorted). That filtering runs in the client's redirect policy before the caller's `CheckRedirect`
(or net/http's default, 10 redirects), so cookies the policy sets on the next request are kept.

## Retry-After

The retry layer honors `Retry-After` on 429 and 503 replies (delta seconds or an HTTP date): the next retry waits at least as long as the server asked,
capped by `WithRetryAfterMax` (default `DefaultRetryAfterMax`, one minute), and never shorter than the exponential backoff. `WithRetryAfterStatuses`
replaces the honored statuses (default `DefaultRetryAfterStatuses()`: 429, 503), e.g. for a retry policy that also retries 500/502/504 replies.
`ParseRetryAfter(h, now, max)` exposes the same parser for callers that run their own retry loop; it reports `ok == false` for an absent or malformed
header.

## Errors

| Error type              | Sentinel                  | Description                                         |
|-------------------------|---------------------------|-----------------------------------------------------|
| `UnexpectedStatusError` | `ErrUnexpectedStatus`     | HTTP response with unexpected 4xx/5xx status code   |
| `CircuitBreakerError`   | `ErrCircuitBreakerOpen`   | Request rejected because circuit breaker is open    |
| `ResponseSizeError`     | `ErrResponseSizeExceeded` | Response Content-Length exceeds configured limit; carries the reply's `Status` and `Header`, never retried |
| `RetryExhaustedError`   | `ErrMaxRetriesExceeded`   | All retry attempts exhausted, wraps last error (see below) |
| `SSRFError`             | `ErrSSRFBlocked`          | Connection to private/local address blocked, or refused by an egress rule (`Rule`) |
| `NonRetryableError`     | `ErrNonRetryable`         | Error that must not be retried (context, TLS, SSRF) |
| --                      | `ErrUninspectableCause`   | Stands for the causes of an error whose cause graph is too large to search (more than 1024 visits: causes shared exponentially, a cause unwrapping to itself); they are dropped |

A request whose retry loop stops on a retryable failure because no further attempt is allowed — the retries, the `WithRetryMaxElapsed` budget or
the delay policy ran out, the context still alive — fails with `RetryExhaustedError` (`Attempts`, `Method`, `URL` without query string or user
info). It wraps the last transport error, or, for a retryable status, the last reply's `UnexpectedStatusError`, so `IsUnexpectedStatusError` reports
the status either way. A failure that is not retried keeps its own error, and a context that ends while waiting for a retry returns the context
error. A `WithErrorHandler` handler receives the `RetryExhaustedError` as its error.

By default the limiter wraps the whole retry loop, so only the first attempt passes it; `WithLimiterPerAttempt()` moves it under the retry layer,
so every attempt waits on a pacing limiter (see [`limiters/hostpacer`](limiters/hostpacer/README.md)) or is refused by a budget — a refusal ends the
request without a retry. With `WithAttemptTimeout` that wait shares the attempt's deadline. A request denied by the `WithLimiter` limiter fails with
[`limiters.RateLimitError`](limiters/README.md)
(`errors.Is(err, limiters.ErrRateLimited)`), which wraps the limiter's error.

## Retry classification

Without `WithRetryPolicyHandler` every attempt is classified by the built-in policy, also exported as `DefaultRetryPolicy` in the
`RetryPolicyHandler` form so a custom policy can delegate to it. A transport error is retried unless another attempt cannot help: the context ended,
an SSRF refusal (an `SSRFError`, or any error matching `ErrSSRFBlocked`, such as a caller's dial policy), a limiter refusal, an open circuit breaker,
an oversized reply (`ResponseSizeError`), an unsupported URL scheme, a rejected server certificate (`x509.UnknownAuthorityError`,
`x509.HostnameError`, `x509.CertificateInvalidError` or any `tls.CertificateVerificationError`), a refused connection, or too many redirects
reported by a transport that follows redirects itself. These errors are matched by type in the bare form a round tripper receives, not only inside a
`*url.Error`; `net/http`'s `Client` enforces its redirect limit above the round tripper, so that failure is never retried. A reply
with a retryable status is retried — `DefaultRetryableStatuses()` (408, 429, 502, 503, 504), or the set of `WithRetryableStatuses` — any other status
of 400 and above fails with `UnexpectedStatusError`, and the rest is returned. `DefaultRetryPolicy` always uses `DefaultRetryableStatuses()`.

```go
c := httpclient.New(httpclient.WithRetryableStatuses(408, 429, 500, 502, 503, 504))
```

The built-in classification replays only requests that are safe to repeat, as `net/http.Transport` does for its own retries: an idempotent method
(GET, HEAD, OPTIONS, TRACE, PUT, DELETE) or a request carrying an `Idempotency-Key` or `X-Idempotency-Key` header. A POST or PATCH without one is sent
once — a server that processed it before failing never sees it twice — and a retryable failure is returned as a `RetryExhaustedError` after that
single attempt. A policy set with `WithRetryPolicyHandler` decides for every method, so it can replay a POST the server deduplicates otherwise.

```go
req.Header.Set("Idempotency-Key", orderID) // the POST is now retried like a GET
```

The Content-Length precheck of `WithMaxResponseSize` closes an oversized reply unread and fails with a `ResponseSizeError` that keeps its `Status` and
`Header`, so a caller can tell an oversized success (a POST that was processed) from an oversized rejection. The limit is its own round tripper
between the circuit breaker and the retry layer, so an oversized reply counts as a breaker success.

## Redaction

Request URLs may carry credentials and signed tokens in their query string. The client never puts the query string or user info into its own errors
and logs: `UnexpectedStatusError.URI` is the escaped path, `RetryExhaustedError.URL` and the retry log's `url` attribute are `scheme://host/path`.
A transport error leaves the retry layer with a message rendered from trusted parts only — this package's error types, fixed sentinel texts, host
names, socket addresses, system error numbers, TLS and certificate errors, `net/http`'s "stopped after N redirects" — because the text of an
arbitrary cause (a `*url.Error`, a custom dialer's error) may embed the full URL; the cause stays reachable, so `errors.Is`/`errors.As` (and
`url.Error.Timeout`) classify it as before. The retry log prints that redacted text. An error a `RetryPolicyHandler` or an `ErrorHandler` returns
(it may quote `resp.Request.URL`) and a failure to buffer or rewind the request body are rendered the same way before they are logged, stored or
returned.

`net/http`'s `Client.Do` wraps every failure in a `*url.Error` that quotes the full request URL, and its nested text may quote a server-supplied URL (an
unparsable redirect `Location`). `RedactError(err)` never modifies `err` (a transport may share it between requests). A `*url.Error` is returned as a
redacted copy: the URL loses its user info, query string and fragment, and the nested error renders trusted text. A wrapper around `*url.Error`s
(`fmt.Errorf` with `%w`, `errors.Join`, a caller's type) may have captured a URL in its own text, so it is returned inside an error whose text is the
redacted `*url.Error`s' only and through which `errors.As` finds the redacted copy; an error whose cause graph is too large to inspect fully gets a
fixed text and wraps `ErrUninspectableCause` instead of its causes. Otherwise causes stay reachable for `errors.Is`/`errors.As` and the `core/errors`
classifiers (`IsResourceRedirects`, `IsUnsupportedProtocolScheme`, `IsCertUnknownAuthority`, `IsConnectionRefused`, `IsRequestTimeoutError`). The
`HTTPClient` methods and `RequestBuilder.Send` apply it already; a caller of the `*http.Client` that `New` returns applies it before logging or
returning an error:

```go
resp, err := client.Get(signedURL)
if err != nil {
    return httpclient.RedactError(err)
}
```

## Interfaces

| Interface          | Description                                                                                                                                                                                                          |
|--------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `HTTPClient`       | What `NewHTTPClient` returns: the composition of `httpx.Doer`, `VerbClient`, `OptionsClient` and `RequestFactory`. Accept the narrowest of them a consumer needs, usually `httpx.Doer`                                |
| `VerbClient`       | Verb helpers: `Get`, `Post`, `PostJSON`, `Put`, `PutJSON`, `Patch`, `PatchJSON`, `Delete`, `Head`, `PostForm`                                                                                                        |
| `OptionsClient`    | Per-call options: `GetWithOptions`, `PostWithOptions`, `PutWithOptions`, `DeleteWithOptions` applying `RequestOption`s                                                                                              |
| `RequestFactory`   | `NewRequest()` returning a fluent `RequestBuilder` sent through the client; its header keys are interned per client (`NewRequestBuilder(doer)` keeps them as given)                                                 |
| `HTTPClientSetter` | Optional `SetHTTPClient(*http.Client)` hook a sub-component implements so a parent can hand it the same shared client (proxy, retry, breaker apply once).                                                           |

## Observer

The client records no metrics and reports no health itself. It delivers its telemetry events to the `Observer` set with `WithObserver` (default
`NoopObserver`, which embeds into partial implementations):

| Event                 | When                                                                                                     |
|-----------------------|----------------------------------------------------------------------------------------------------------|
| `Attach`              | once, from `New`, with a read-only `Breakers` view (`States`, `HostState`, `ConfiguredHosts`)            |
| `RequestDone`         | a request completed after all its attempts: method, host, status class, retries, failure, duration       |
| `Retry`               | a retry is about to be made, before its backoff sleep                                                    |
| `BreakerRejected`     | an attempt was rejected by an open (or saturated half-open) circuit breaker                              |
| `BreakerStateChanged` | a breaker changed state; runs with the breaker's lock held, so it must not read `Breakers` synchronously |

Embed `NoopObserver` to implement only the events of interest:

```go
type retryCounter struct {
    client.NoopObserver
    retries atomic.Int64
}

func (o *retryCounter) Retry(client.AttemptEvent) { o.retries.Add(1) }

c := client.New(client.WithObserver(&retryCounter{}))
```

## Subpackages

| Package                | Description                                                               |
|------------------------|---------------------------------------------------------------------------|
| [limiters](limiters)   | Pluggable rate limiting layer for HTTP round-trippers                     |

## SSRF and proxy policy

SSRF protection is enabled by default and blocks private, link-local and loopback destinations after DNS resolution. A NAT64 address in
`64:ff9b::/96` is judged by the IPv4 address it translates to, and the local-use NAT64 prefix `64:ff9b:1::/48` is blocked. Allow local development
explicitly with `WithSSRFAllowedCIDRs(netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128"))`.

Protected clients disable environment proxy lookup. An explicit proxy (`WithProxy`, `WithProxyURL`, `WithProxyFunc`) is allowed: the connection to the
proxy goes through the guarded dialer, so a proxy on a private address needs its network in `WithSSRFAllowedCIDRs`; the proxy resolves and dials the
target host, so a private target named by host name is not blocked by the client — enforce egress restrictions at the proxy. A target given as a literal
IP address in a blocked range is refused with an `SSRFError` before it reaches the proxy. A proxy TLS configuration (`WithProxyTLSConfig`), custom TLS
dialers, a proxy resolver set on a caller transport and custom round trippers cannot enforce the policy; `New` and `NewHTTPClient` panic with an error
wrapping `ErrSSRFUnsafeTransport` when given these configurations. Use `WithoutSSRFProtection()` for trusted proxy/custom-transport clients and enforce
destination restrictions in that transport or proxy. Caller-owned clients and transports are cloned before policy changes.

`SSRFDialControl(allowed...)` exports the dial guard (a `net.Dialer.Control` hook) for services that build their own transport: it rejects private,
loopback, link-local and reserved resolved addresses with an `SSRFError`, checks (and exempts) IPv4-mapped addresses as IPv4 only and ignores zones.

### Egress policy

`NewEgressPolicy(allow, deny []EgressRule)` compiles rules once; an `EgressRule` matches a host glob (`path.Match` syntax, case-insensitive, `*` also
matches dots), a `netip.Prefix` and a port, ANDed, zero matchers matching anything. A destination is refused when a deny rule matches (deny wins),
allowed when an allow rule matches, and otherwise refused only when its resolved address is private, loopback, link-local or reserved. Allow rules only
exempt addresses from that default denial, so each needs a CIDR; host and port narrow it. A deny rule needs at least one matcher. Invalid rules fail
with `ErrInvalidEgressRule`. An IPv4-mapped address (`::ffff:a.b.c.d`) is decided as the IPv4 address it connects to — net.Dialer and proxydial both
reach it over IPv4 — so IPv4 rules match it and IPv6 prefixes, `::/0` included, match IPv6 connections only; a policy decides an address alike in either
form. An IPv4-mapped prefix of length 96 or more is the equivalent IPv4 prefix, a shorter one is rejected; zones are ignored. Internationalized names
match in the ASCII (punycode) form net/http dials: a rule written `bücher.example` matches `xn--bcher-kva.example` and `bücher.example` alike, on a
direct dial and before a proxy; a Unicode label holding a glob metacharacter is rejected, write it in punycode. The Unicode full stops IDNA reads as
dots (U+3002, U+FF0E, U+FF61) separate labels in rules and hosts alike, a trailing one included.

The rule grammar lives in [`egressrule`](../../egressrule/README.md), so a configuration schema that uses it validates a rule exactly
as the engine compiles it. `ParseEgressPolicy(allow, deny []egressrule.Rule)` compiles rules in their textual
form (`name`, `host`, `cidr` string, `port` int), as configured or stored; it also rejects a CIDR that does not parse and a port outside 0 to 65535,
with errors wrapping `ErrInvalidEgressRule` and naming the rule (`allow[0].cidr`). `WithEgressPolicy` installs the compiled
policy.

| Install                                 | Where the check runs                                                                                                  |
|-----------------------------------------|-----------------------------------------------------------------------------------------------------------------------|
| `WithEgressPolicy(policy)`              | The client's dial control hook, on each resolved address actually connected to; `WithSSRFAllowedCIDRs` still applies |
| `policy.DialContext(dialer)`            | A transport the caller builds: a copy of `dialer` checks each resolved address, then runs the dialer's own hook       |
| `proxydial.WithSSRFGuard(policy.Check)` | Proxied dials: the destination is resolved locally, every address checked, and the proxy asked for the approved IP    |

Every refusal is an `*SSRFError` matching `ErrSSRFBlocked` and is never retried; for a deny rule `SSRFError.Rule` names it (`deny[0] host=*.internal`).
`EgressRule.Name` replaces the positional name in that label (`deny[7]`, `allowPrivate`), so a caller compiling a subset of its own rules can keep its
numbering; it is not a matcher, and it must be 1 to 64 characters of `A-Z a-z 0-9 _ . - [ ]`, so the label survives the redaction of client errors.
`NormalizeEgressHostGlob(glob)` validates a host glob and returns the form the policy matches (lower case, punycode labels, Unicode full stops as dots);
the normalized form is validated again, so a glob such as `example.` plus a soft hyphen, which IDNA maps to `example.`, is rejected. Hand rules on to
another enforcer (a proxy, a browser) in this form. `policy.Explain(host, dst)` returns the `EgressDecision` behind `Check`: `Allowed`, a `Reason`
(`EgressDenyRule`, `EgressAllowRule`, `EgressPrivateDefault`, `EgressPublic`, `EgressInvalidAddress`; `String()` gives `deny_rule`, ...), the matching
rule's label for deny and allow rules (`allowedCIDRs[i]` for `WithSSRFAllowedCIDRs`), the host and the decided (unmapped) address; `Err()` is the
`Check` result. `policy.CheckHost(host, port)` decides a destination before its host is resolved: a host name against the deny rules without a CIDR, a
literal IP host (in any form net/http sends) in full. With an explicit client proxy, a target host name is resolved by the proxy and checked only
against the deny rules without a CIDR, while a literal IP target is checked in full. To also resolve and pin proxied targets, build the transport with
[`proxydial`](../../proxydial/README.md) and `WithSSRFGuard`.

```go
policy, err := client.NewEgressPolicy(
    []client.EgressRule{{CIDR: netip.MustParsePrefix("10.20.0.0/16"), Host: "*.corp.example"}},
    []client.EgressRule{{Host: "*.internal"}, {CIDR: netip.MustParsePrefix("169.254.0.0/16")}, {Port: 25}},
)
if err != nil {
    return err
}
c := client.New(client.WithEgressPolicy(policy))
```
