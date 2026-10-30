// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package client

//go:generate optgen generate --type=options

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/altessa-s/go-atlas-net/http/client/limiters"
)

func defaultClient() *http.Client {
	return defaultPooledClient()
}

// defaultPooledClient returns a new [http.Client] with a pooled transport
// configured identically to go-cleanhttp's DefaultPooledClient: connection
// pooling, keep-alive, TLS handshake timeout, HTTP/2, and per-host idle
// connections scaled to GOMAXPROCS.
func defaultPooledClient() *http.Client {
	return &http.Client{
		Transport: DefaultPooledTransport(),
	}
}

// CircuitBreakerSettings defines per-host circuit breaker configuration.
// Supply it via [WithCircuitBreakerSettings] to override the global breaker
// defaults for a specific hostname. Zero-value fields fall back to the
// package-level defaults ([DefaultBreakerMaxRequests], [DefaultBreakerInterval],
// [DefaultBreakerTimeout]). Use [DefaultOnStateChange] for a ready-made
// state-transition logging callback.
type CircuitBreakerSettings struct {
	// MaxRequests is the maximum number of requests allowed in half-open state
	MaxRequests uint32
	// Interval is the period to clear failure counts in closed state
	Interval time.Duration
	// Timeout is the period after which circuit breaker transitions from open to half-open
	Timeout time.Duration
	// Name is the circuit breaker name for identification
	Name string
	// ReadyToTrip is the function to determine when the circuit breaker should trip
	ReadyToTrip func(counts gobreaker.Counts) bool
	// OnStateChange is called when the circuit breaker state changes
	OnStateChange func(name string, from gobreaker.State, to gobreaker.State)
}

// options holds the internal configuration state for the HTTP client.
// This struct is not exported and is modified through Option functions.
type options struct {
	client             *http.Client `optgen:"default=defaultClient()"`
	transport          *http.Transport
	retryMax           int           `optgen:"default=DefaultRetryMax"`
	breakerMaxRequests uint32        `optgen:"default=DefaultBreakerMaxRequests"`
	retryWaitMin       time.Duration `optgen:"default=DefaultRetryWaitMin"`
	retryWaitMax       time.Duration `optgen:"default=DefaultRetryWaitMax"`
	// retryAfterMax caps the Retry-After of a 429/503 reply the retry layer
	// honors: the next retry waits at least that long (beyond retryWaitMax),
	// up to this cap. Zero or negative ignores Retry-After.
	retryAfterMax time.Duration `optgen:"default=DefaultRetryAfterMax" optval:"nonpositive"`
	// retryFactor is the exponential growth of the retry backoff per attempt.
	retryFactor float64 `optgen:"default=DefaultRetryFactor"`
	// retryJitter is the fraction (0-1) of each backoff added as random
	// jitter; zero makes the delays deterministic.
	retryJitter float64 `optgen:"default=DefaultRetryJitter" optval:"nonpositive"`
	// retryMaxElapsed bounds the wall-clock time of all attempts of one
	// request; zero or negative leaves it to the context and retryMax.
	retryMaxElapsed time.Duration `optval:"nonpositive"`
	// attemptTimeout bounds each attempt of the retry layer — the limiter
	// wait under limiterPerAttempt, the connection, the reply and reading
	// its body — rather than the whole request: retries and their backoff
	// (Retry-After included) are not cut short by the first attempt. Zero or
	// negative leaves attempts bounded by the request context and
	// http.Client.Timeout.
	attemptTimeout  time.Duration `optval:"nonpositive"`
	breakerInterval time.Duration `optgen:"default=DefaultBreakerInterval"`
	breakerTimeout  time.Duration `optgen:"default=DefaultBreakerTimeout"`
	breakerName     string
	// maxHostBreakers caps the number of per-host breakers created lazily for
	// hosts without explicit [CircuitBreakerSettings]. Once reached, further
	// hosts share the client-wide breaker. Zero or negative disables lazy
	// per-host breakers entirely.
	maxHostBreakers    int `optgen:"default=DefaultMaxHostBreakers"`
	maxResponseSize    int64
	errorHandler       ErrorHandler
	retryPolicyHandler RetryPolicyHandler
	// withoutRetry disables the retry layer: every request is sent exactly
	// once and its outcome is returned as it is — the response for any
	// status, body unread, or the transport error (its message redacted, its
	// causes intact). It overrides retryMax
	// and retryPolicyHandler. Set it with the generated WithoutRetry when the
	// caller classifies replies itself or retries around the client.
	withoutRetry bool `opt:"Retry" optval:"invert"`
	// withoutRedirects stops the client from following redirects: a 3xx
	// reply is returned to the caller as the response, body unread
	// (CheckRedirect returns http.ErrUseLastResponse). It is applied to the
	// client before the proxy, SSRF and circuit-breaker layers copy it, so
	// every layer keeps it, and it replaces the CheckRedirect of a client
	// supplied with WithClient (on a copy; the caller's client is unchanged).
	// Set it with the generated WithoutRedirects.
	withoutRedirects bool `opt:"Redirects" optval:"invert"`
	logger           *slog.Logger
	limiter          limiters.RequestsLimiter `optgen:"notnil"`
	// limiterPerAttempt, set by the generated WithLimiterPerAttempt, places
	// the limiter under the retry layer instead of around it, so every retry
	// attempt waits on (or is refused by) the limiter too, not only the
	// first. A refusal is never retried by the default classification.
	limiterPerAttempt bool
	// hostBreakerSettings maps hostnames to their circuit breaker settings
	hostBreakerSettings map[string]*CircuitBreakerSettings `opt:"-"`
	// ssrfProtection enables blocking connections to private/local IP addresses.
	// Defaults to DefaultSSRFProtection (on); opt out with WithoutSSRFProtection.
	ssrfProtection bool `optgen:"manual,default=DefaultSSRFProtection"`
	// ssrfAllowedCIDRs contains CIDR prefixes exempted from SSRF blocking
	ssrfAllowedCIDRs []netip.Prefix `optgen:"manual"`
	// egressPolicy, set by the generated WithEgressPolicy, replaces the
	// built-in private-address check of SSRF protection with an allow/deny
	// [EgressPolicy] (deny wins; allow rules exempt private ranges). The
	// ssrfAllowedCIDRs exemptions still apply on top of it. It has an effect
	// only while SSRF protection is on (the default): WithoutSSRFProtection
	// disables it too. With an explicit proxy, a target host name is checked
	// against the deny rules that need no address, a literal IP target in
	// full; the proxy resolves host names.
	egressPolicy *EgressPolicy
	// observer receives the client's telemetry events (see [Observer]).
	// Defaults to [NoopObserver]; set it with the generated WithObserver.
	observer Observer `optgen:"default=defaultObserver(),notnil"`
	// proxy resolves the proxy URL for outgoing requests. The auto-generated
	// setter is named WithProxyFunc to free the WithProxy name for the more
	// common host/port/auth convenience setter declared below.
	proxy ProxyFunc `opt:"ProxyFunc"`
	// proxyTLSConfig customizes the TLS handshake to https:// proxies
	// (e.g. a corporate proxy fronted by a self-signed cert). When set,
	// the client routes outbound requests through a custom DialContext
	// that performs TCP+TLS+CONNECT manually, isolating proxy TLS from
	// the destination's TLSClientConfig. Has no effect for plain http://
	// or socks5:// proxies.
	proxyTLSConfig *tls.Config
	// retryAfterStatuses are the reply statuses whose Retry-After the retry
	// layer honors (see retryAfterMax). Defaults to 429 and 503.
	retryAfterStatuses []int `optgen:"default=DefaultRetryAfterStatuses()"`
	// retryableStatuses are the reply statuses the built-in retry
	// classification retries; it applies when no retryPolicyHandler is set.
	// Defaults to DefaultRetryableStatuses(): 408, 429, 502, 503 and 504.
	retryableStatuses []int `optgen:"default=DefaultRetryableStatuses()"`
	// cookieJar is the cookie jar the retry layer owns. Every attempt and
	// every redirect hop sends the caller's Cookie header plus the jar's
	// cookies for its URL, read at attempt time, and every reply's cookies
	// are stored exactly once, when received — retried, failed and redirect
	// replies included. When unset, the jar of a WithClient client is used
	// the same way; the returned *http.Client has no Jar either way.
	cookieJar http.CookieJar
}

// WithRetryWait configures the minimum and maximum wait times between retry attempts.
// The actual wait time uses exponential backoff with jitter between these bounds.
// Defaults are 500ms minimum and 10s maximum.
//
// Example:
//
//	client := httpclient.New(
//	    httpclient.WithRetryWait(1*time.Second, 30*time.Second),
//	)
func WithRetryWait(minDur, maxDur time.Duration) Option {
	return func(opts *options) {
		opts.retryWaitMin = minDur
		opts.retryWaitMax = maxDur
	}
}

// WithCircuitBreakerSettings configures circuit breaker settings for a specific hostname.
// This allows different hosts to have different circuit breaker configurations.
// The hostname should match the host part of the URL (e.g., "api.example.com").
//
// Example:
//
//	client := httpclient.New(
//	    httpclient.WithCircuitBreakerSettings("api.example.com", &httpclient.CircuitBreakerSettings{
//	        MaxRequests: 5,
//	        Interval:    60 * time.Second,
//	        Timeout:     30 * time.Second,
//	        Name:        "api-example-cb",
//	    }),
//	    httpclient.WithCircuitBreakerSettings("backup.example.com", &httpclient.CircuitBreakerSettings{
//	        MaxRequests: 3,
//	        Interval:    30 * time.Second,
//	        Timeout:     15 * time.Second,
//	        Name:        "backup-example-cb",
//	    }),
//	)
func WithCircuitBreakerSettings(hostname string, settings *CircuitBreakerSettings) Option {
	return func(opts *options) {
		if opts.hostBreakerSettings == nil {
			opts.hostBreakerSettings = make(map[string]*CircuitBreakerSettings)
		}
		opts.hostBreakerSettings[hostname] = settings
	}
}

// DefaultSSRFProtection is the default value of the SSRF-protection toggle:
// protection is ON by default, blocking connections to private and local IP
// addresses. Opt a specific client out with [WithoutSSRFProtection].
const DefaultSSRFProtection = true

// WithSSRFProtection enables SSRF protection that blocks connections to
// private IP addresses (RFC1918, link-local, ULA, cloud metadata, etc.).
// Loopback (127.0.0.0/8, ::1) is blocked too. The check runs after DNS
// resolution but before the TCP connection is established, preventing both
// direct private-IP requests and DNS rebinding attacks.
//
// SSRF protection is ON by default ([DefaultSSRFProtection]); this option is
// only needed to re-enable it after a [WithoutSSRFProtection]. Use
// [WithSSRFAllowedCIDRs] to exempt known internal networks. Environment proxies
// are disabled; explicit proxy options are guarded as described on [ProxyFunc].
// A proxy TLS configuration, custom TLS dialers, a proxy resolver set on a
// caller transport and non-standard transports cause construction to panic
// with an error wrapping [ErrSSRFUnsafeTransport].
//
// Example:
//
//	client := httpclient.New(
//	    httpclient.WithSSRFAllowedCIDRs(netip.MustParsePrefix("10.0.1.0/24")),
//	)
func WithSSRFProtection() Option {
	return func(opts *options) {
		opts.ssrfProtection = true
	}
}

// WithoutSSRFProtection disables the default SSRF protection, letting the client
// connect to private and local IP addresses. Use for internal service-to-service
// clients that must reach private hosts and cannot enumerate them via
// [WithSSRFAllowedCIDRs].
//
// Example:
//
//	client := httpclient.New(
//	    httpclient.WithoutSSRFProtection(),
//	)
func WithoutSSRFProtection() Option {
	return func(opts *options) {
		opts.ssrfProtection = false
	}
}

// WithSSRFAllowedCIDRs sets CIDR prefixes that are exempted from SSRF blocking.
// Use this for known internal service-to-service communication that must reach
// private addresses, including loopback. Protection is enabled by default.
//
// Example:
//
//	client := httpclient.New(
//	    httpclient.WithSSRFProtection(),
//	    httpclient.WithSSRFAllowedCIDRs(
//	        netip.MustParsePrefix("10.0.1.0/24"), // internal service network
//	    ),
//	)
func WithSSRFAllowedCIDRs(cidrs ...netip.Prefix) Option {
	return func(opts *options) {
		opts.ssrfAllowedCIDRs = append(opts.ssrfAllowedCIDRs, cidrs...)
	}
}

// ProxyFunc resolves the proxy URL for an outgoing request. It matches the
// signature of [http.Transport.Proxy]: returning (nil, nil) means "no proxy
// for this request"; returning a non-nil error aborts the request before
// dialing.
//
// With SSRF protection on (the default) an explicit proxy is allowed: the
// connection to the proxy goes through the guarded dialer, so a proxy on a
// private address needs its network in [WithSSRFAllowedCIDRs]. The proxy
// resolves and dials the target host, so private targets named by host name
// are not blocked by the client: enforce egress restrictions at the proxy. A
// target given as a literal IP address in a blocked range is refused with an
// [SSRFError] before the request reaches the proxy. With [WithEgressPolicy] a
// host name target is also checked against the deny rules that need no
// address; to resolve and pin targets locally, dial through proxydial with
// its WithSSRFGuard option instead.
type ProxyFunc = func(*http.Request) (*url.URL, error)

// WithProxy routes all requests through an HTTP proxy at host:port with
// optional credentials. Pass nil auth for an anonymous proxy; build auth
// with [url.UserPassword] or [url.User]. The scheme is always http — for
// https/socks5 proxies use [WithProxyURL] instead. Setting this disables
// the default HTTP_PROXY/HTTPS_PROXY/NO_PROXY environment lookup.
//
// Invalid host or port produce a resolver that fails on the first request
// rather than silently falling back to the default proxy. The proxy is
// only applied when the underlying transport is a [*http.Transport] —
// custom round-trippers supplied via [WithClient] are left untouched.
//
// Example:
//
//	httpclient.New(httpclient.WithoutSSRFProtection(),
//	    httpclient.WithProxy("proxy.corp", 3128, url.UserPassword("svc", token)))
func WithProxy(host string, port int, auth *url.Userinfo) Option {
	if host == "" {
		return WithProxyFunc(failingProxy(errors.New("httpclient: WithProxy: empty host")))
	}
	if port < 1 || port > 65535 {
		return WithProxyFunc(failingProxy(fmt.Errorf("httpclient: WithProxy: invalid port %d", port)))
	}
	u := &url.URL{
		Scheme: "http",
		User:   auth,
		Host:   net.JoinHostPort(host, strconv.Itoa(port)),
	}
	return WithProxyFunc(http.ProxyURL(u))
}

// WithProxyURL routes all requests through the given proxy URL. Supported
// schemes are http, https, socks5, and socks5h. Credentials embedded in
// the URL (http://user:pass@host:port) trigger Proxy-Authorization
// automatically. Setting this disables the default
// HTTP_PROXY/HTTPS_PROXY/NO_PROXY environment lookup.
//
// A nil URL produces a resolver that fails on the first request rather
// than silently falling back to the default proxy.
func WithProxyURL(u *url.URL) Option {
	if u == nil {
		return WithProxyFunc(failingProxy(errors.New("httpclient: WithProxyURL: nil URL")))
	}
	return WithProxyFunc(http.ProxyURL(u))
}

// WithoutProxy disables proxy resolution entirely, including the default
// environment-based lookup. Use when the process runs in an environment
// where HTTP_PROXY is set but specific clients must connect directly.
func WithoutProxy() Option {
	return WithProxyFunc(noProxyResolver)
}

// noProxyResolver always reports "no proxy", overriding the default
// environment-based lookup baked into [DefaultPooledTransport].
// Returning (nil, nil) is the contract documented on [http.Transport.Proxy]
// — a nil URL means "use no proxy for this request".
//
//nolint:nilnil // (nil, nil) is the http.Transport.Proxy "no proxy" contract.
func noProxyResolver(*http.Request) (*url.URL, error) { return nil, nil }

// failingProxy returns a resolver that always fails with err. Used by
// the convenience proxy options when their inputs are invalid so that
// misconfiguration surfaces at request time instead of being masked by
// the default env-based proxy.
func failingProxy(err error) ProxyFunc {
	return func(*http.Request) (*url.URL, error) { return nil, err }
}

// DefaultOnStateChange returns a [gobreaker.Settings.OnStateChange] callback that
// logs circuit breaker state transitions at debug level using the provided logger.
// Pass this to [CircuitBreakerSettings.OnStateChange] to get visibility into breaker
// behavior without writing a custom callback.
func DefaultOnStateChange(logger *slog.Logger) func(name string, from, to gobreaker.State) {
	return func(name string, from, to gobreaker.State) {
		if logger != nil {
			//nolint:contextcheck // callback from gobreaker library has no request context
			logger.DebugContext(context.Background(),
				fmt.Sprintf("circuit [%s] change state from %s to %s", name, from, to))
		}
	}
}
