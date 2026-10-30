// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package client

import (
	"cmp"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/altessa-s/go-atlas-core/text/strings/intern"

	"github.com/altessa-s/go-atlas-net/httpx"

	coreerrs "github.com/altessa-s/go-atlas-core/errors"
)

const (
	// DefaultBreakerTimeout is the default timeout for the circuit breaker to transition from open to half-open state
	DefaultBreakerTimeout = 30 * time.Second
	// DefaultBreakerInterval is the default interval for clearing failure counts in closed state
	DefaultBreakerInterval = 30 * time.Second
	// DefaultBreakerMaxRequests is the default number of requests allowed in half-open state
	DefaultBreakerMaxRequests = 3
	// hostnameInternerSize is the size of the string interner for hostnames.
	hostnameInternerSize = 1024

	// DefaultBreakerMinRequests is the minimum number of requests before evaluating failure ratio.
	// The circuit breaker requires at least this many requests in the closed state
	// before it will consider opening based on failure ratio.
	DefaultBreakerMinRequests = 3

	// DefaultBreakerFailureRatioThreshold is the failure ratio threshold (0.0 to 1.0).
	// When the failure ratio exceeds this threshold and minimum requests are met,
	// the circuit breaker will trip to open state. Default is 0.6 (60%).
	DefaultBreakerFailureRatioThreshold = 0.6

	// DefaultBreakerConsecutiveFailuresLimit is the maximum consecutive failures allowed.
	// When consecutive failures reach this limit, the circuit breaker will trip
	// to open state regardless of the failure ratio.
	DefaultBreakerConsecutiveFailuresLimit = 100

	// DefaultMaxHostBreakers is the maximum number of per-host circuit breakers
	// created lazily for hosts without explicit [CircuitBreakerSettings]. It
	// bounds memory and the number of breakers an [Observer] tracks when
	// request URLs come from untrusted input; hosts beyond the cap share the
	// client-wide breaker.
	DefaultMaxHostBreakers = 1024
)

var (
	// circuitBreakerCounter is used to generate unique names for circuit breakers
	circuitBreakerCounter atomic.Uint64
)

type circuitBreakerClient struct {
	*gobreaker.CircuitBreaker[*http.Response]
	client   *http.Client
	logger   *slog.Logger
	observer Observer
	// hostBreakers maps hostnames to their circuit breakers; it is the only
	// per-host breaker cache
	hostBreakers map[string]*gobreaker.CircuitBreaker[*http.Response]
	// hostBreakerSettings contains per-host circuit breaker settings
	hostBreakerSettings map[string]*CircuitBreakerSettings
	// mu protects access to hostBreakers map and lazyBreakers
	mu sync.RWMutex
	// lazyBreakers counts entries of hostBreakers created without explicit settings
	lazyBreakers int
	// maxHostBreakers caps lazyBreakers; see [WithMaxHostBreakers]
	maxHostBreakers int
	// sharedUsed reports whether any request has been routed through the
	// client-wide breaker, so health aggregation can ignore it while idle
	sharedUsed atomic.Bool
	// lazy breakers inherit the client-wide breaker settings
	breakerMaxRequests uint32
	breakerInterval    time.Duration
	breakerTimeout     time.Duration
	// stringInterner provides efficient string interning for hostnames
	stringInterner *intern.Interner
}

// defaultReadyToTrip determines when the circuit breaker should trip to open state.
// It trips when minimum requests are met and either failure ratio exceeds threshold
// or consecutive failures reach the limit.
func defaultReadyToTrip(counts gobreaker.Counts) bool {
	failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
	return counts.Requests >= DefaultBreakerMinRequests &&
		(failureRatio >= DefaultBreakerFailureRatioThreshold ||
			counts.ConsecutiveFailures >= DefaultBreakerConsecutiveFailuresLimit)
}

// applyProxy applies the proxy override, if configured. It clones the
// underlying transport before mutating its Proxy field so neither a
// *http.Transport supplied via WithTransport nor a *http.Client supplied via
// WithClient is modified in place. A nil Transport is normalized to
// http.DefaultTransport so callers passing &http.Client{} still see the proxy
// applied (otherwise net/http would fall back to http.DefaultTransport with
// http.ProxyFromEnvironment, silently defeating WithoutProxy / WithProxy /
// WithProxyURL). Custom round-trippers that are not *http.Transport are left
// alone.
func applyProxy(httpClient *http.Client, opts options) *http.Client {
	if opts.proxy == nil {
		return httpClient
	}
	t, ok := transportOf(httpClient).(*http.Transport)
	if !ok {
		return httpClient
	}
	cloned := t.Clone()
	if opts.proxyTLSConfig != nil {
		// Custom DialContext handles TCP+TLS+CONNECT manually
		// so the proxy TLS material stays isolated from the
		// destination's TLSClientConfig. Stdlib's
		// Transport.Proxy must be nil — our dialer resolves
		// the proxy via opts.proxy itself.
		cloned.Proxy = nil
		cloned.DialContext = proxyAwareDialer(opts.proxy, opts.proxyTLSConfig)
	} else {
		cloned.Proxy = opts.proxy
	}
	return withClientTransport(httpClient, opts.client, cloned)
}

// applySSRFProtection wraps the transport with SSRF protection, if enabled.
// Like [applyProxy], a client still aliasing the caller's WithClient client
// is cloned before its Transport changes.
func applySSRFProtection(httpClient *http.Client, opts options) *http.Client {
	if !opts.ssrfProtection {
		return httpClient
	}
	t, ok := transportOf(httpClient).(*http.Transport)
	if !ok {
		return httpClient
	}
	wrapped := newSSRFSafeTransport(t, opts.ssrfAllowedCIDRs, opts.proxy, opts.egressPolicy)
	return withClientTransport(httpClient, opts.client, wrapped)
}

// transportOf returns the client's transport, normalizing nil to
// http.DefaultTransport.
func transportOf(c *http.Client) http.RoundTripper {
	if c.Transport == nil {
		return http.DefaultTransport
	}
	return c.Transport
}

// withClientTransport sets transport on httpClient. When httpClient still
// aliases the caller's client, it returns a copy instead, so the caller's
// *http.Client is never modified in place.
func withClientTransport(httpClient, callerClient *http.Client, transport http.RoundTripper) *http.Client {
	if httpClient != callerClient {
		httpClient.Transport = transport
		return httpClient
	}
	return &http.Client{
		Transport:     transport,
		CheckRedirect: httpClient.CheckRedirect,
		Jar:           httpClient.Jar,
		Timeout:       httpClient.Timeout,
	}
}

func newCircuitBreakerClient(opts options) *circuitBreakerClient {
	httpClient := opts.client
	if httpClient == nil {
		httpClient = defaultPooledClient()
	}

	// Disable redirects first, on a copy, so every client derived below
	// inherits it and a WithClient client is never modified in place.
	if opts.withoutRedirects {
		httpClient = &http.Client{
			Transport:     httpClient.Transport,
			CheckRedirect: useLastResponse,
			Jar:           httpClient.Jar,
			Timeout:       httpClient.Timeout,
		}
	}

	// Apply transport if provided
	if opts.transport != nil {
		httpClient = &http.Client{
			Transport:     opts.transport,
			CheckRedirect: httpClient.CheckRedirect,
			Jar:           httpClient.Jar,
			Timeout:       httpClient.Timeout,
		}
	}

	httpClient = applyProxy(httpClient, opts)
	httpClient = applySSRFProtection(httpClient, opts)

	// Generate circuit breaker name
	var cbName string
	if opts.breakerName != "" {
		cbName = opts.breakerName
	} else {
		cbName = fmt.Sprintf("httpclient-cb-%d", circuitBreakerCounter.Add(1))
	}

	client := &circuitBreakerClient{
		client:              httpClient,
		logger:              opts.logger,
		observer:            opts.observer,
		hostBreakers:        make(map[string]*gobreaker.CircuitBreaker[*http.Response]),
		hostBreakerSettings: opts.hostBreakerSettings,
		maxHostBreakers:     opts.maxHostBreakers,
		breakerMaxRequests:  opts.breakerMaxRequests,
		breakerInterval:     opts.breakerInterval,
		breakerTimeout:      opts.breakerTimeout,

		// Create dedicated string interner for HTTP client with smaller size
		// HTTP client typically has predictable string patterns (hostnames, common headers)
		stringInterner: intern.New(hostnameInternerSize), // Smaller than global default (8192)
	}
	observer := opts.observer
	// nolint:bodyclose
	client.CircuitBreaker = gobreaker.NewCircuitBreaker[*http.Response](gobreaker.Settings{
		Name:        cbName,
		MaxRequests: opts.breakerMaxRequests,
		Interval:    opts.breakerInterval,
		Timeout:     opts.breakerTimeout,
		ReadyToTrip: defaultReadyToTrip,
		OnStateChange: func(name string, from, to gobreaker.State) {
			observer.BreakerStateChanged(BreakerStateEvent{Name: name, From: from, To: to})
		},
	})

	return client
}

// internHostname returns an interned version of the hostname for memory optimization.
// Uses a dedicated string interner for HTTP client to ensure identical hostnames share the same memory location.
// Provides LRU eviction and hot caching specifically for HTTP hostnames.
func (c *circuitBreakerClient) internHostname(hostname string) string {
	return c.stringInterner.String(hostname)
}

// getBreakerForHost returns the circuit breaker for a given hostname.
// Every host gets its own breaker so failures of one endpoint never reject
// requests to another: hosts with explicit [CircuitBreakerSettings] use those
// settings, other hosts get a lazily created breaker that inherits the
// client-wide settings. Once [WithMaxHostBreakers] lazy breakers exist, new
// hosts share the client-wide breaker.
func (c *circuitBreakerClient) getBreakerForHost(hostname string) *gobreaker.CircuitBreaker[*http.Response] {
	// Intern hostname for memory optimization
	hostname = c.internHostname(hostname)

	settings, explicit := c.hostBreakerSettings[hostname]

	// Fast path: an already created breaker, under the read lock
	c.mu.RLock()
	breaker, exists := c.hostBreakers[hostname]
	full := !explicit && c.lazyBreakers >= c.maxHostBreakers
	c.mu.RUnlock()

	if exists {
		return breaker
	}
	if full {
		c.sharedUsed.Store(true)
		return c.CircuitBreaker
	}

	// Create new breaker for this host
	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check in case another goroutine created it
	breaker, exists = c.hostBreakers[hostname]
	if exists {
		return breaker
	}
	if !explicit && c.lazyBreakers >= c.maxHostBreakers {
		c.sharedUsed.Store(true)
		return c.CircuitBreaker
	}

	var st gobreaker.Settings
	var userCb func(name string, from gobreaker.State, to gobreaker.State)
	if explicit {
		// Set defaults if not provided
		st = gobreaker.Settings{
			Name:        settings.Name,
			MaxRequests: cmp.Or(settings.MaxRequests, DefaultBreakerMaxRequests),
			Interval:    cmp.Or(settings.Interval, DefaultBreakerInterval),
			Timeout:     cmp.Or(settings.Timeout, DefaultBreakerTimeout),
			ReadyToTrip: settings.ReadyToTrip,
		}
		if st.Name == "" {
			st.Name = fmt.Sprintf("httpclient-cb-%s-%d", hostname, circuitBreakerCounter.Add(1))
		}
		userCb = settings.OnStateChange
	} else {
		st = gobreaker.Settings{
			Name:        c.Name() + "-" + hostname,
			MaxRequests: c.breakerMaxRequests,
			Interval:    c.breakerInterval,
			Timeout:     c.breakerTimeout,
		}
	}
	if st.ReadyToTrip == nil {
		st.ReadyToTrip = defaultReadyToTrip
	}

	st.OnStateChange = func(n string, from gobreaker.State, to gobreaker.State) {
		c.observer.BreakerStateChanged(BreakerStateEvent{Name: n, Host: hostname, From: from, To: to})
		if userCb != nil {
			userCb(n, from, to)
		}
	}

	// nolint:bodyclose
	breaker = gobreaker.NewCircuitBreaker[*http.Response](st)

	c.hostBreakers[hostname] = breaker
	if !explicit {
		c.lazyBreakers++
	}
	return breaker
}

func (c *circuitBreakerClient) standardClient() *http.Client {
	next := c.client.Transport
	if next == nil {
		next = http.DefaultTransport
	}

	return &http.Client{
		Transport:     c.newCircuitBreakerRoundTripper(next),
		CheckRedirect: c.client.CheckRedirect,
		Jar:           c.client.Jar,
		Timeout:       c.client.Timeout,
	}
}

// idleClosingRoundTripper pairs a wrapping round-tripper with the transport
// it wraps so [http.Client.CloseIdleConnections] reaches the pooled transport.
type idleClosingRoundTripper struct {
	httpx.RoundTripperFunc
	next http.RoundTripper
}

// CloseIdleConnections forwards to the wrapped transport.
func (rt idleClosingRoundTripper) CloseIdleConnections() { closeIdleConnections(rt.next) }

// closeIdleConnections closes rt's idle connections when it supports it, the
// same contract [http.Client.CloseIdleConnections] applies to its Transport.
func closeIdleConnections(rt http.RoundTripper) {
	if c, ok := rt.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// newCircuitBreakerRoundTripper creates a new round tripper with host-specific circuit breaker support
func (c *circuitBreakerClient) newCircuitBreakerRoundTripper(next http.RoundTripper) http.RoundTripper {
	return idleClosingRoundTripper{next: next, RoundTripperFunc: func(req *http.Request) (*http.Response, error) {
		// Get the appropriate circuit breaker for this host
		breaker := c.getBreakerForHost(req.URL.Host)

		res, err := breaker.Execute(func() (*http.Response, error) {
			res, err := next.RoundTrip(req) //nolint:bodyclose
			// The transport's error is rendered from trusted parts, and its
			// cause graph bounded, before anything formats or searches it.
			return res, redactTransportError(err)
		})

		if err != nil {
			// The URL omits user info and the query string: both may carry
			// credentials.
			return nil, coreerrs.Wrapf(err, "circuit breaker execution failed for %s %s", req.Method, redactedURL(req.URL))
		}

		return res, nil
	}}
}

// useLastResponse is the CheckRedirect of [WithoutRedirects]: never follow a
// redirect, return the 3xx reply as the response.
func useLastResponse(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
