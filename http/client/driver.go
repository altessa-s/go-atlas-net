// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/altessa-s/go-atlas-net/http/client/transportpool"
	"github.com/altessa-s/go-atlas-net/proxydial"
)

// DefaultDialTimeout bounds driver connection setup, including proxy tunnels.
const DefaultDialTimeout = 30 * time.Second

// TransportDriver builds an HTTPS transport for one proxy route. It is trusted
// in-process code, not a security sandbox: every connection, including reconnects,
// fallback and resumed sessions, MUST come from env.Dial. Source review of all
// connection-opening paths and drivertest coverage establish compliance.
//
// Every non-default TLS field must be honored with crypto/tls semantics or
// rejected by NewTransport. In particular this includes RootCAs, ServerName,
// MinVersion, MaxVersion, NextProtos, Certificates, GetClientCertificate,
// VerifyPeerCertificate and VerifyConnection. The core rejects InsecureSkipVerify.
// Drivers own TLS, ALPN dispatch, connection pooling and protocol framing, must
// support concurrent requests, and must implement CloseIdleConnections.
// NewTransport must not open connections. Direct-route errors panic in New;
// errors constructing subsequent proxy routes are returned by RoundTrip.
// Requests that may have been processed and non-rewindable bodies must not be
// replayed. Proven unprocessed requests (HTTP/2 REFUSED_STREAM or GOAWAY below
// their stream ID) with rewindable bodies may be retried, including with
// WithoutRetry. These retries are bounded by the request, not a transmission
// count: persistent refusal must return an error promptly when the request
// context expires or is canceled, and retrying must stop when the request ends.
type TransportDriver interface {
	// NewTransport builds a transport using only the supplied connection path.
	NewTransport(env DriverEnv) (http.RoundTripper, error)
}

// DriverEnv contains the core-owned connection path and destination settings.
type DriverEnv struct {
	// Dial returns a guarded direct connection or a locally pinned proxy tunnel.
	// Its deadline bounds setup only and is cleared before returning a connection.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
	// TLS is a clone of the destination TLS configuration; nil means defaults.
	TLS *tls.Config
	// MaxIdleConns is the maximum idle connection count across authorities.
	MaxIdleConns int
	// MaxIdleConnsPerHost is the maximum idle connection count per authority.
	MaxIdleConnsPerHost int
	// MaxConnsPerHost bounds all connections per authority; zero is unlimited.
	MaxConnsPerHost int
	// IdleConnTimeout bounds how long an unused connection may remain idle.
	IdleConnTimeout time.Duration
	// TLSHandshakeTimeout bounds destination TLS handshakes.
	TLSHandshakeTimeout time.Duration
	// ResponseHeaderTimeout bounds the wait for response headers.
	ResponseHeaderTimeout time.Duration
	// ExpectContinueTimeout bounds the wait for a 100 Continue reply.
	ExpectContinueTimeout time.Duration
	// DisableKeepAlives disables connection reuse.
	DisableKeepAlives bool
	// DisableCompression disables automatic compression negotiation.
	DisableCompression bool
}

type driverRouteKey struct{}
type driverRoute struct{ proxy *url.URL }

type driverTransport struct {
	base      *http.Transport
	proxy     ProxyFunc
	pool      *transportpool.Pool
	initialMu sync.Mutex
	initial   http.RoundTripper
}

// validateDriverTransport runs before SSRF replaces the caller's dialer and
// before Clone can initialize the standard transport's protocol configuration.
func validateDriverTransport(opts *options) error {
	if opts.transportDriver == nil {
		return nil
	}

	transport := transportOf(opts.client)
	if opts.transport != nil {
		transport = opts.transport
	}

	base, ok := transport.(*http.Transport)
	if !ok || base == nil {
		return fmt.Errorf("%w: requires a non-nil *http.Transport", ErrDriverUnsupportedTransport)
	}

	var field string

	switch {
	case base.Dial != nil && base.DialContext == nil: //nolint:staticcheck // Reject legacy dials that cannot honor cancellation.
		field = "Dial without DialContext"
	case base.ProxyConnectHeader != nil:
		field = "ProxyConnectHeader"
	case base.GetProxyConnectHeader != nil:
		field = "GetProxyConnectHeader"
	case base.OnProxyConnectResponse != nil:
		field = "OnProxyConnectResponse"
	case base.DialTLS != nil: //nolint:staticcheck // Reject legacy TLS dials as well.
		field = "DialTLS"
	case base.DialTLSContext != nil:
		field = "DialTLSContext"
	case base.Protocols != nil:
		field = "Protocols"
	case base.HTTP2 != nil:
		field = "HTTP2"
	case base.TLSNextProto != nil:
		field = "TLSNextProto"
	case base.MaxResponseHeaderBytes != 0:
		field = "MaxResponseHeaderBytes"
	default:
		return nil
	}

	return fmt.Errorf("%w: %s cannot be honored by the driver connection path", ErrDriverUnsupportedTransport, field)
}

func applyTransportDriver(c *http.Client, opts options) *http.Client {
	base, ok := transportOf(c).(*http.Transport)
	if !ok || base == nil {
		panic("httpclient: transport driver requires a non-nil *http.Transport")
	}

	if base.TLSClientConfig != nil && base.TLSClientConfig.InsecureSkipVerify {
		panic("httpclient: transport driver rejects InsecureSkipVerify")
	}

	base = base.Clone()
	if opts.ssrfProtection {
		base = newSSRFSafeTransport(base, opts.ssrfAllowedCIDRs, opts.proxy, opts.egressPolicy)
	} else if opts.proxy != nil {
		base.Proxy = opts.proxy
	}
	// Keep the standard HTTP path, while resolving HTTPS routes before pooling.
	httpBase := base.Clone()
	// This transport serves only HTTP destinations, so its TLS policy is solely
	// for HTTPS proxies. Never apply destination roots or callbacks to a proxy.
	httpBase.TLSClientConfig = nil
	if opts.proxyTLSConfig != nil {
		httpBase.TLSClientConfig = opts.proxyTLSConfig.Clone()
	}

	d := &driverTransport{base: httpBase, proxy: base.Proxy}
	build := func(u *url.URL) (http.RoundTripper, error) {
		dial, err := driverDial(base, opts, u)
		if err != nil {
			return nil, err
		}

		env := DriverEnv{Dial: dial, MaxIdleConns: base.MaxIdleConns, MaxIdleConnsPerHost: base.MaxIdleConnsPerHost,
			MaxConnsPerHost: base.MaxConnsPerHost, IdleConnTimeout: base.IdleConnTimeout,
			TLSHandshakeTimeout: base.TLSHandshakeTimeout, ResponseHeaderTimeout: base.ResponseHeaderTimeout,
			ExpectContinueTimeout: base.ExpectContinueTimeout, DisableKeepAlives: base.DisableKeepAlives, DisableCompression: base.DisableCompression}
		if base.TLSClientConfig != nil {
			env.TLS = base.TLSClientConfig.Clone()
		}

		tr, err := opts.transportDriver.NewTransport(env)
		if err == nil && tr == nil {
			err = errors.New("httpclient: driver returned nil transport")
		}

		return tr, err
	}

	initial, err := build(nil)
	if err != nil {
		panic(fmt.Errorf("httpclient: construct transport driver: %w", err))
	}

	d.initial = initial
	d.pool = transportpool.New(func(ctx context.Context) (string, func() http.RoundTripper) {
		route, _ := ctx.Value(driverRouteKey{}).(driverRoute)

		key := "direct"
		if route.proxy != nil {
			key = "proxy:" + route.proxy.String()
		}

		return key, func() http.RoundTripper {
			if route.proxy == nil {
				d.initialMu.Lock()
				tr := d.initial
				d.initial = nil
				d.initialMu.Unlock()

				if tr != nil {
					return tr
				}
			}

			tr, err := build(route.proxy)
			if err != nil {
				return driverErrorTransport{err}
			}

			return tr
		}
	})

	return withClientTransport(c, opts.client, d)
}

func (d *driverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != schemeHTTPS {
		return d.base.RoundTrip(req)
	}

	var u *url.URL

	if d.proxy != nil {
		var err error

		u, err = d.proxy(req)
		if err != nil {
			if req.Body != nil {
				_ = req.Body.Close()
			}

			return nil, err
		}
	}

	if u != nil {
		copyURL := *u
		u = &copyURL
	}

	ctx := context.WithValue(req.Context(), driverRouteKey{}, driverRoute{proxy: u})

	return d.pool.RoundTrip(req.WithContext(ctx))
}

func (d *driverTransport) CloseIdleConnections() {
	d.base.CloseIdleConnections()
	d.pool.CloseIdleConnections()

	d.initialMu.Lock()
	initial := d.initial
	d.initialMu.Unlock()

	if c, ok := initial.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

type driverErrorTransport struct{ err error }

func (d driverErrorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}

	return nil, d.err
}

func driverDial(base *http.Transport, opts options, u *url.URL) (proxydial.DialContextFunc, error) {
	dial := base.DialContext

	if dial == nil {
		dial = proxydial.DefaultDialer().DialContext
	}

	if u != nil {
		copyURL := *u
		if copyURL.Port() == "" {
			port := "1080"

			switch copyURL.Scheme {
			case schemeHTTP:
				port = "80"
			case schemeHTTPS:
				port = "443"
			}

			copyURL.Host = net.JoinHostPort(copyURL.Hostname(), port)
		}

		u = &copyURL
		pd := proxydial.DefaultDialer()

		var guard proxydial.AddrGuard

		proxyDial := dial

		if opts.ssrfProtection {
			policy := opts.egressPolicy
			if policy == nil {
				policy = &EgressPolicy{}
			}

			policy = policy.withAllowedCIDRs(opts.ssrfAllowedCIDRs)
			pd.ControlContext = policy.control(u.Hostname(), nil, nil)
			proxyDial = pd.DialContext
			guard = policy.Check
		}

		var err error

		dial, err = proxydial.FromURL(u, proxydial.WithDialer(pd), proxydial.WithSSRFGuard(guard),
			proxydial.WithProxyTLSConfig(opts.proxyTLSConfig), proxydial.WithDialContext(proxyDial))
		if err != nil {
			return nil, err
		}
	}

	timeout := opts.dialTimeout
	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}

	return func(ctx context.Context, network, address string) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		conn, err := dial(ctx, network, address)
		if err != nil {
			if conn != nil {
				_ = conn.Close()
			}

			return nil, err
		}

		if conn == nil {
			return nil, errors.New("httpclient: driver dial returned nil connection")
		}

		if err = ctx.Err(); err == nil {
			err = conn.SetDeadline(time.Time{})
		}

		if err != nil {
			_ = conn.Close()
			return nil, err
		}

		return conn, nil
	}, nil
}
