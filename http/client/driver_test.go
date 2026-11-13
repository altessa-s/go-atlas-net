// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"
	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/http/client/limiters"
)

type driverFunc func(DriverEnv) (http.RoundTripper, error)

func (f driverFunc) NewTransport(e DriverEnv) (http.RoundTripper, error) { return f(e) }

func standardDriver(e DriverEnv) (http.RoundTripper, error) {
	return &http.Transport{DialContext: e.Dial, TLSClientConfig: e.TLS, ForceAttemptHTTP2: true,
		MaxIdleConns: e.MaxIdleConns, MaxIdleConnsPerHost: e.MaxIdleConnsPerHost, MaxConnsPerHost: e.MaxConnsPerHost,
		IdleConnTimeout: e.IdleConnTimeout, TLSHandshakeTimeout: e.TLSHandshakeTimeout, ResponseHeaderTimeout: e.ResponseHeaderTimeout}, nil
}
func driverBase(s *httptest.Server) *http.Transport {
	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())

	tr := DefaultPooledTransport()
	tr.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}

	return tr
}
func driverGet(t *testing.T, c *http.Client, target string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "GET", target, nil)
	require.NoError(t, err)

	resp, err := c.Do(req)
	if resp != nil {
		_, readErr := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		if err == nil {
			err = readErr
		}
	}

	return resp, err
}

// tunnelProxy serves CONNECT or SOCKS5 locally, and returns accepted tunnel count.
func tunnelProxy(t *testing.T, scheme string) (*url.URL, *atomic.Int32) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var (
		hits atomic.Int32
		wg   sync.WaitGroup
	)

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})
	go func() {
		defer close(done)

		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			wg.Go(func() {
				defer conn.Close()

				stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
				defer stop()

				br := bufio.NewReader(conn)

				var target string

				if scheme == "http" {
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}

					_ = req.Body.Close()
					target = req.Host
				} else {
					header := make([]byte, 2)
					if _, err := io.ReadFull(br, header); err != nil {
						return
					}

					methods := make([]byte, int(header[1]))
					if _, err := io.ReadFull(br, methods); err != nil {
						return
					}

					if _, err := conn.Write([]byte{5, 0}); err != nil {
						return
					}

					header = make([]byte, 4)
					if _, err := io.ReadFull(br, header); err != nil {
						return
					}

					var host string

					switch header[3] {
					case 1:
						ip := make([]byte, 4)
						if _, err := io.ReadFull(br, ip); err != nil {
							return
						}

						host = net.IP(ip).String()
					case 4:
						ip := make([]byte, 16)
						if _, err := io.ReadFull(br, ip); err != nil {
							return
						}

						host = net.IP(ip).String()
					case 3:
						n, err := br.ReadByte()
						if err != nil {
							return
						}

						name := make([]byte, int(n))
						if _, err := io.ReadFull(br, name); err != nil {
							return
						}

						host = string(name)
					default:
						return
					}

					port := make([]byte, 2)
					if _, err := io.ReadFull(br, port); err != nil {
						return
					}

					target = net.JoinHostPort(host, fmtPort(binary.BigEndian.Uint16(port)))
				}

				upstream, err := (&net.Dialer{}).DialContext(ctx, "tcp", target)
				if err != nil {
					return
				}
				defer upstream.Close()

				hits.Add(1)

				if scheme == "http" {
					_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
				} else {
					_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				}

				copied := make(chan struct{})

				go func() { _, _ = io.Copy(upstream, br); _ = upstream.Close(); close(copied) }()

				_, _ = io.Copy(conn, upstream)
				_ = conn.Close()

				<-copied
			})
		}
	}()

	t.Cleanup(func() { cancel(); _ = ln.Close(); <-done; wg.Wait() })

	return &url.URL{Scheme: scheme, Host: ln.Addr().String()}, &hits
}
func fmtPort(p uint16) string { return strconv.Itoa(int(p)) }

func TestDriverRouteIsolation(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer s.Close()

	a, ac := tunnelProxy(t, "http")
	b, bc := tunnelProxy(t, "socks5")

	var (
		route    *url.URL
		resolves atomic.Int32
	)

	c := New(WithTransport(driverBase(s)), WithTransportDriver(driverFunc(standardDriver)), WithoutRetry(),
		WithSSRFAllowedCIDRs(netip.MustParsePrefix("127.0.0.0/8")), WithProxyFunc(func(r *http.Request) (*url.URL, error) {
			require.Equal(t, "/selected", r.URL.Path)
			resolves.Add(1)

			return route, nil
		}))
	defer c.CloseIdleConnections()

	for _, u := range []*url.URL{a, b, nil, a, nil, b} {
		route = u
		_, err := driverGet(t, c, s.URL+"/selected")
		require.NoError(t, err)
	}

	require.EqualValues(t, 6, resolves.Load())
	require.EqualValues(t, 1, ac.Load())
	require.EqualValues(t, 1, bc.Load())

	userA, userB := *a, *a
	userA.User = url.UserPassword("a", "one")

	userB.User = url.UserPassword("b", "two")
	for _, u := range []*url.URL{&userA, &userB, &userA} {
		route = u
		_, err := driverGet(t, c, s.URL+"/selected")
		require.NoError(t, err)
	}

	require.EqualValues(t, 3, ac.Load(), "user identity must isolate tunnels")
	c.CloseIdleConnections()

	route = a
	_, err := driverGet(t, c, s.URL+"/selected")
	require.NoError(t, err)
	require.EqualValues(t, 4, ac.Load())
}

func TestDriverDialDeadline(t *testing.T) {
	for _, scheme := range []string{"https", "http", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)

			defer ln.Close()

			done := make(chan struct{})
			go func() {
				defer close(done)

				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()

				if scheme == "socks5" {
					b := make([]byte, 3)
					_, _ = io.ReadFull(conn, b)
					_, _ = conn.Write([]byte{5, 0})
				}

				_, _ = io.Copy(io.Discard, conn)
			}()

			opts := *newOptions(WithDialTimeout(80*time.Millisecond), WithSSRFAllowedCIDRs(netip.MustParsePrefix("127.0.0.0/8")))
			dial, err := driverDial(DefaultPooledTransport(), opts, &url.URL{Scheme: scheme, Host: ln.Addr().String()})
			require.NoError(t, err)

			start := time.Now()
			conn, err := dial(context.Background(), "tcp", "127.0.0.1:443")
			require.Error(t, err)
			require.Nil(t, conn)
			require.Less(t, time.Since(start), time.Second)

			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("failed tunnel leaked connection")
			}
		})
	}

	t.Run("earlier caller deadline", func(t *testing.T) {
		var seen time.Time

		base := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			seen, _ = ctx.Deadline()
			<-ctx.Done()

			return nil, ctx.Err()
		}}
		dial, err := driverDial(base, *newOptions(WithoutSSRFProtection(), WithDialTimeout(time.Second)), nil)
		require.NoError(t, err)

		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()

		_, err = dial(ctx, "tcp", "unused:443")
		require.ErrorIs(t, err, context.DeadlineExceeded)

		want, _ := ctx.Deadline()
		require.Equal(t, want, seen)
	})
}

func TestDriverTunnelDeadlineCleared(t *testing.T) {
	for _, scheme := range []string{"http", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)

			defer ln.Close()

			ready := make(chan net.Conn, 1)

			go func() {
				conn, err := ln.Accept()
				if err == nil {
					ready <- conn
				}
			}()

			proxy, _ := tunnelProxy(t, scheme)
			opts := *newOptions(WithDialTimeout(30*time.Millisecond), WithSSRFAllowedCIDRs(netip.MustParsePrefix("127.0.0.0/8")))
			dial, err := driverDial(DefaultPooledTransport(), opts, proxy)
			require.NoError(t, err)
			conn, err := dial(context.Background(), "tcp", ln.Addr().String())
			require.NoError(t, err)

			defer conn.Close()

			peer := <-ready
			defer peer.Close()
			// Wait for the former deadline, then perform I/O. This timer tests expiry,
			// rather than coordinating goroutines by sleeping.
			<-time.After(60 * time.Millisecond)
			require.NoError(t, peer.SetDeadline(time.Now().Add(time.Second)))

			wrote := make(chan error, 1)

			go func() { _, err := peer.Write([]byte("ok")); wrote <- err }()

			b := make([]byte, 2)
			_, err = io.ReadFull(conn, b)
			require.NoError(t, err)
			require.NoError(t, <-wrote)
			require.Equal(t, "ok", string(b))
		})
	}
}

func TestDriverGuardAndRedirect(t *testing.T) {
	for _, scheme := range []string{"direct", "http", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			var connected atomic.Int32

			driver := driverFunc(func(e DriverEnv) (http.RoundTripper, error) {
				dial := e.Dial
				e.Dial = func(ctx context.Context, n, a string) (net.Conn, error) {
					c, err := dial(ctx, n, a)
					if err == nil {
						connected.Add(1)
					}

					return c, err
				}

				return standardDriver(e)
			})
			opts := []Option{WithTransportDriver(driver), WithoutRetry(), WithoutProxy()}

			if scheme != "direct" {
				proxy, hits := tunnelProxy(t, scheme)
				opts = append(opts, WithProxyURL(proxy))

				defer func() { require.Zero(t, hits.Load()) }()
			}

			c := New(opts...)
			defer c.CloseIdleConnections()

			_, err := driverGet(t, c, "https://127.0.0.1:443")
			require.ErrorIs(t, err, ErrSSRFBlocked)
			require.Zero(t, connected.Load())
		})
	}

	t.Run("deny and redirect", func(t *testing.T) {
		s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://127.0.0.2:443", http.StatusFound)
		}))
		defer s.Close()

		p, err := NewEgressPolicy(nil, []EgressRule{{Host: "127.0.0.2"}})
		require.NoError(t, err)

		c := New(WithTransportDriver(driverFunc(standardDriver)), WithTransport(driverBase(s)), WithEgressPolicy(p),
			WithSSRFAllowedCIDRs(netip.MustParsePrefix("127.0.0.0/8")), WithoutRetry(), WithoutProxy())
		defer c.CloseIdleConnections()

		_, err = driverGet(t, c, s.URL)
		require.ErrorIs(t, err, ErrSSRFBlocked)
		_, err = driverGet(t, c, "https://127.0.0.2:443")
		require.ErrorIs(t, err, ErrSSRFBlocked)
	})
}

func TestDriverConstructionAndHTTP(t *testing.T) {
	t.Run("fail closed", func(t *testing.T) {
		sentinel := errors.New("unsupported TLS setting")

		require.Panics(t, func() {
			New(WithTransportDriver(driverFunc(func(DriverEnv) (http.RoundTripper, error) { return nil, sentinel })))
		})
		require.Panics(t, func() {
			New(WithTransportDriver(driverFunc(standardDriver)), WithTransport(&http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}))
		})
		require.Panics(t, func() {
			New(WithTransportDriver(driverFunc(standardDriver)), WithTransport(&http.Transport{DialTLSContext: func(context.Context, string, string) (net.Conn, error) { return nil, sentinel }}))
		})
	})
	t.Run("http bypasses driver", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
		defer s.Close()

		c := New(WithTransportDriver(driverFunc(func(DriverEnv) (http.RoundTripper, error) {
			return driverErrorTransport{errors.New("driver used for HTTP")}, nil
		})),
			WithoutRetry(), WithSSRFAllowedCIDRs(netip.MustParsePrefix("127.0.0.0/8")))
		defer c.CloseIdleConnections()

		_, err := driverGet(t, c, s.URL)
		require.NoError(t, err)
	})
	t.Run("clone and tuning", func(t *testing.T) {
		tr := DefaultPooledTransport()
		tr.TLSClientConfig = &tls.Config{ServerName: "original", MinVersion: tls.VersionTLS13}
		tr.ResponseHeaderTimeout = time.Second
		tr.MaxConnsPerHost = 7
		c := New(WithTransport(tr), WithTransportDriver(driverFunc(func(e DriverEnv) (http.RoundTripper, error) {
			require.NotSame(t, tr.TLSClientConfig, e.TLS)
			require.Equal(t, "original", e.TLS.ServerName)
			require.Equal(t, time.Second, e.ResponseHeaderTimeout)
			require.Equal(t, 7, e.MaxConnsPerHost)
			e.TLS.ServerName = "changed"

			return standardDriver(e)
		})))
		c.CloseIdleConnections()
		require.Equal(t, "original", tr.TLSClientConfig.ServerName)
	})
}

func TestDriverWrappers(t *testing.T) {
	for _, name := range []string{"retry", "limiter", "size", "breaker", "cookies"} {
		t.Run(name, func(t *testing.T) {
			var (
				hits, calls atomic.Int32
				cookie      atomic.Bool
			)

			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := hits.Add(1)

				if name == "cookies" {
					if _, err := r.Cookie("session"); err == nil {
						cookie.Store(true)
					}

					http.SetCookie(w, &http.Cookie{Name: "session", Value: "yes"})
				}

				if (name == "retry" || name == "limiter") && n == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}

				if name == "breaker" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}

					return
				}

				_, _ = io.WriteString(w, "response body")
			}))
			defer s.Close()

			opts := []Option{WithTransportDriver(driverFunc(standardDriver)), WithTransport(driverBase(s)), WithoutProxy(),
				WithSSRFAllowedCIDRs(netip.MustParsePrefix("127.0.0.0/8")), WithRetryMax(2), WithRetryWait(time.Millisecond, time.Millisecond)}

			switch name {
			case "limiter":
				opts = append(opts, WithLimiterPerAttempt(), WithLimiter(limiters.Func(func(context.Context, string) error { calls.Add(1); return nil })))
			case "size":
				opts = append(opts, WithMaxResponseSize(3))
			case "breaker":
				opts = append(opts, WithoutRetry(), WithCircuitBreakerSettings(s.Listener.Addr().String(), &CircuitBreakerSettings{ReadyToTrip: func(gobreaker.Counts) bool { return true }}))
			case "cookies":
				jar, err := cookiejar.New(nil)
				require.NoError(t, err)

				opts = append(opts, WithCookieJar(jar), WithoutRetry())
			}

			c := New(opts...)
			defer c.CloseIdleConnections()

			_, err := driverGet(t, c, s.URL)

			switch name {
			case "retry":
				require.NoError(t, err)
				require.EqualValues(t, 2, hits.Load())
			case "limiter":
				require.NoError(t, err)
				require.EqualValues(t, 2, calls.Load())
			case "size":
				require.Error(t, err)
				require.EqualValues(t, 1, hits.Load())
			case "breaker":
				_, err = driverGet(t, c, s.URL)
				require.True(t, IsCircuitBreakerOpen(err))
				require.EqualValues(t, 1, hits.Load())
			case "cookies":
				require.NoError(t, err)
				_, err = driverGet(t, c, s.URL)
				require.NoError(t, err)
				require.True(t, cookie.Load())
			}
		})
	}
}

func TestDriverProxyLocalResolution(t *testing.T) {
	for _, scheme := range []string{"http", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			proxy, hits := tunnelProxy(t, scheme)
			policy, err := NewEgressPolicy(nil, []EgressRule{
				{CIDR: netip.MustParsePrefix("127.0.0.0/8"), Port: 443},
				{CIDR: netip.MustParsePrefix("::1/128"), Port: 443},
			})
			require.NoError(t, err)

			var connected atomic.Int32

			d := driverFunc(func(e DriverEnv) (http.RoundTripper, error) {
				dial := e.Dial
				e.Dial = func(ctx context.Context, n, a string) (net.Conn, error) {
					c, err := dial(ctx, n, a)
					if err == nil {
						connected.Add(1)
					}

					return c, err
				}

				return standardDriver(e)
			})

			c := New(WithTransportDriver(d), WithProxyURL(proxy), WithEgressPolicy(policy), WithoutRetry(),
				WithSSRFAllowedCIDRs(netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")))
			defer c.CloseIdleConnections()

			_, err = driverGet(t, c, "https://localhost:443")
			require.ErrorIs(t, err, ErrSSRFBlocked)
			require.Zero(t, connected.Load())
			require.Zero(t, hits.Load())
		})
	}
}

func TestDriverHTTPSProxyTLS(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer target.Close()

	var tunnels atomic.Int32

	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream, err := (&net.Dialer{}).DialContext(r.Context(), "tcp", r.Host)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer upstream.Close()

		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()

		tunnels.Add(1)

		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		done := make(chan struct{})

		go func() { _, _ = io.Copy(upstream, rw); _ = upstream.Close(); close(done) }()

		_, _ = io.Copy(conn, upstream)
		_ = conn.Close()

		<-done
	}))
	defer proxy.Close()

	roots := x509.NewCertPool()
	roots.AddCert(proxy.Certificate())

	var proxyVerified atomic.Int32

	proxyTLS := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, VerifyConnection: func(tls.ConnectionState) error { proxyVerified.Add(1); return nil }}
	u, err := url.Parse(proxy.URL)
	require.NoError(t, err)

	c := New(WithTransportDriver(driverFunc(standardDriver)), WithTransport(driverBase(target)), WithProxyURL(u), WithProxyTLSConfig(proxyTLS), WithDialTimeout(100*time.Millisecond),
		WithoutRetry(), WithSSRFAllowedCIDRs(netip.MustParsePrefix("127.0.0.0/8")))
	defer c.CloseIdleConnections()

	_, err = driverGet(t, c, target.URL)
	require.NoError(t, err)
	require.EqualValues(t, 1, tunnels.Load())
	require.EqualValues(t, 1, proxyVerified.Load())
	require.Empty(t, proxyTLS.ServerName)
	// The established HTTPS tunnel must outlive its setup deadline.
	<-time.After(150 * time.Millisecond)

	_, err = driverGet(t, c, target.URL)
	require.NoError(t, err)
	require.EqualValues(t, 1, tunnels.Load())
}

func TestDriverProxyErrorsBeforeSelection(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer s.Close()

	var (
		route    *url.URL
		proxyErr error
		builds   atomic.Int32
	)

	c := New(WithTransport(driverBase(s)), WithTransportDriver(driverFunc(func(e DriverEnv) (http.RoundTripper, error) { builds.Add(1); return standardDriver(e) })),
		WithoutRetry(), WithSSRFAllowedCIDRs(netip.MustParsePrefix("127.0.0.0/8")), WithProxyFunc(func(*http.Request) (*url.URL, error) { return route, proxyErr }))
	defer c.CloseIdleConnections()

	_, err := driverGet(t, c, s.URL)
	require.NoError(t, err)
	// An invalid proxy string must not collide with the cached direct route.
	route = &url.URL{Path: "direct"}
	_, err = driverGet(t, c, s.URL)
	require.Error(t, err)
	require.EqualValues(t, 1, builds.Load())

	proxyErr = errors.New("resolver refused")
	_, err = driverGet(t, c, s.URL)
	require.ErrorIs(t, err, proxyErr)
	require.EqualValues(t, 1, builds.Load())
}

func TestDriverUnguardedBaseDial(t *testing.T) {
	for _, scheme := range []string{"direct", "http", "https", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			sentinel := errors.New("base dial refused")

			var address string

			base := &http.Transport{DialContext: func(_ context.Context, _, a string) (net.Conn, error) { address = a; return nil, sentinel }}

			opts := []Option{WithoutSSRFProtection(), WithoutRetry(), WithoutProxy(), WithTransport(base), WithTransportDriver(driverFunc(standardDriver))}
			if scheme != "direct" {
				opts = append(opts, WithProxyURL(&url.URL{Scheme: scheme, Host: "proxy.invalid"}))
			}

			c := New(opts...)
			defer c.CloseIdleConnections()

			_, err := driverGet(t, c, "https://destination.invalid")
			require.ErrorIs(t, err, sentinel)

			switch scheme {
			case "direct":
				require.Equal(t, "destination.invalid:443", address)
			case "http":
				require.Equal(t, "proxy.invalid:80", address)
			case "https":
				require.Equal(t, "proxy.invalid:443", address)
			case "socks5":
				require.Equal(t, "proxy.invalid:1080", address)
			}
		})
	}
}

func TestDriverHTTPProxyTLS(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(strconv.FormatBool(reject), func(t *testing.T) {
			var calls atomic.Int32

			proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "http://destination.invalid/", r.URL.String())
				w.WriteHeader(http.StatusOK)
			}))
			defer proxy.Close()

			proxyURL, err := url.Parse(proxy.URL)
			require.NoError(t, err)

			sentinel := errors.New("proxy verification rejected")
			proxyTLS := driverBase(proxy).TLSClientConfig
			proxyTLS.VerifyConnection = func(tls.ConnectionState) error {
				calls.Add(1)

				if reject {
					return sentinel
				}

				return nil
			}
			destinationTLS := &tls.Config{RootCAs: x509.NewCertPool(), ServerName: "destination.invalid", MinVersion: tls.VersionTLS13,
				VerifyConnection: func(tls.ConnectionState) error { return errors.New("destination callback used for proxy") }}

			c := New(WithTransport(&http.Transport{TLSClientConfig: destinationTLS}), WithTransportDriver(driverFunc(func(e DriverEnv) (http.RoundTripper, error) {
				require.Equal(t, destinationTLS.ServerName, e.TLS.ServerName)
				require.Same(t, destinationTLS.RootCAs, e.TLS.RootCAs)

				return standardDriver(e)
			})), WithProxyFunc(http.ProxyURL(proxyURL)), WithProxyTLSConfig(proxyTLS), WithSSRFAllowedCIDRs(netip.MustParsePrefix("127.0.0.0/8")), WithoutRetry())
			defer c.CloseIdleConnections()

			_, err = driverGet(t, c, "http://destination.invalid/")
			if reject {
				require.ErrorIs(t, err, sentinel)
			} else {
				require.NoError(t, err)
			}

			require.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestDriverLegacyDial(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		t.Run(strconv.FormatBool(proxy), func(t *testing.T) {
			sentinel := errors.New("legacy dial refused")

			var address string

			base := &http.Transport{
				Dial:        func(_, _ string) (net.Conn, error) { return nil, errors.New("unexpected legacy dial") },
				DialContext: func(_ context.Context, _, addr string) (net.Conn, error) { address = addr; return nil, sentinel },
			}
			opts := []Option{WithTransport(base), WithTransportDriver(driverFunc(standardDriver)), WithoutSSRFProtection(), WithoutProxy(), WithoutRetry()}
			want := "destination.invalid:443"

			if proxy {
				u := &url.URL{Scheme: "http", Host: "proxy.invalid:8080"}
				opts = append(opts, WithProxyFunc(http.ProxyURL(u)))
				want = u.Host
			}

			c := New(opts...)
			defer c.CloseIdleConnections()

			_, err := driverGet(t, c, "https://destination.invalid/")
			require.ErrorIs(t, err, sentinel)
			require.Equal(t, want, address)
		})
	}
}

func TestDriverUnsupportedTransport(t *testing.T) {
	dial := func(string, string) (net.Conn, error) { return nil, errors.New("unexpected dial") }

	tests := []struct {
		name      string
		configure func(*http.Transport)
	}{
		{"Dial without DialContext", func(tr *http.Transport) { tr.Dial = dial }},
		{"ProxyConnectHeader", func(tr *http.Transport) { tr.ProxyConnectHeader = http.Header{"X-Test": {"value"}} }},
		{"ProxyConnectHeader empty", func(tr *http.Transport) { tr.ProxyConnectHeader = http.Header{} }},
		{"GetProxyConnectHeader", func(tr *http.Transport) {
			tr.GetProxyConnectHeader = func(context.Context, *url.URL, string) (http.Header, error) { return nil, nil }
		}},
		{"OnProxyConnectResponse", func(tr *http.Transport) {
			tr.OnProxyConnectResponse = func(context.Context, *url.URL, *http.Request, *http.Response) error { return nil }
		}},
		{"DialTLS", func(tr *http.Transport) { tr.DialTLS = dial }},
		{"DialTLSContext", func(tr *http.Transport) {
			tr.DialTLSContext = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("unexpected dial") }
		}},
		{"Protocols", func(tr *http.Transport) { tr.Protocols = new(http.Protocols) }},
		{"HTTP2", func(tr *http.Transport) { tr.HTTP2 = new(http.HTTP2Config) }},
		{"TLSNextProto", func(tr *http.Transport) { tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{} }},
		{"MaxResponseHeaderBytes", func(tr *http.Transport) { tr.MaxResponseHeaderBytes = 1024 }},
	}
	for _, tt := range tests {
		for _, ssrf := range []bool{false, true} {
			for _, viaClient := range []bool{false, true} {
				t.Run(tt.name+"/ssrf="+strconv.FormatBool(ssrf)+"/client="+strconv.FormatBool(viaClient), func(t *testing.T) {
					base := &http.Transport{}
					tt.configure(base)

					opts := []Option{WithoutProxy(), WithTransportDriver(driverFunc(func(DriverEnv) (http.RoundTripper, error) {
						t.Fatal("driver constructed before validation")
						return nil, nil
					}))}
					if !ssrf {
						opts = append(opts, WithoutSSRFProtection())
					}

					if viaClient {
						opts = append(opts, WithClient(&http.Client{Transport: base}))
					} else {
						opts = append(opts, WithTransport(base))
					}

					defer func() {
						err, ok := recover().(error)
						require.True(t, ok, "construction must panic with an error")
						require.ErrorIs(t, err, ErrDriverUnsupportedTransport)
						require.Contains(t, err.Error(), strings.TrimSuffix(tt.name, " empty"))
					}()

					New(opts...)
				})
			}
		}
	}
}

// A rejecting proxy lets us observe the standard CONNECT hooks without a TLS
// handshake or an external connection. Callback errors must survive wrapping.
func TestConnectConfigurationWithoutDriver(t *testing.T) {
	for _, setting := range []string{"ProxyConnectHeader", "GetProxyConnectHeader", "OnProxyConnectResponse"} {
		t.Run(setting, func(t *testing.T) {
			var headerSeen, callbackSeen atomic.Bool

			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				headerSeen.Store(r.Method == http.MethodConnect && r.Header.Get("X-Test") == "value")
				w.WriteHeader(http.StatusForbidden)
			}))
			defer proxy.Close()

			u, err := url.Parse(proxy.URL)
			require.NoError(t, err)

			base := &http.Transport{}
			sentinel := errors.New("CONNECT response refused")

			switch setting {
			case "ProxyConnectHeader":
				base.ProxyConnectHeader = http.Header{"X-Test": {"value"}}
			case "GetProxyConnectHeader":
				base.GetProxyConnectHeader = func(context.Context, *url.URL, string) (http.Header, error) {
					callbackSeen.Store(true)
					return http.Header{"X-Test": {"value"}}, nil
				}
			case "OnProxyConnectResponse":
				base.OnProxyConnectResponse = func(_ context.Context, _ *url.URL, _ *http.Request, res *http.Response) error {
					callbackSeen.Store(res.StatusCode == http.StatusForbidden)
					return sentinel
				}
			}

			c := New(WithTransport(base), WithoutSSRFProtection(), WithProxyFunc(http.ProxyURL(u)), WithoutRetry())
			defer c.CloseIdleConnections()

			_, err = driverGet(t, c, "https://destination.invalid/")
			require.Error(t, err)

			if setting == "OnProxyConnectResponse" {
				require.ErrorIs(t, err, sentinel)
			} else {
				require.True(t, headerSeen.Load())
			}

			if setting != "ProxyConnectHeader" {
				require.True(t, callbackSeen.Load())
			}
		})
	}
}
