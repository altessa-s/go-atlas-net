// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package drivertest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/http/client"
)

const testTimeout = 3 * time.Second
const shortTimeout = 100 * time.Millisecond
const authority = "driver.invalid"

// Run tests a fresh driver from factory in each subtest. All traffic is local.
// Configurations explicitly permitted to fail closed are accepted when
// NewTransport returns an error. A driver that accepts them must enforce them.
func Run(t *testing.T, factory func(*testing.T) client.TransportDriver) {
	t.Helper()
	t.Run("DialOwnership", func(t *testing.T) { testDial(t, factory) })
	t.Run("Verification", func(t *testing.T) { testVerification(t, factory) })
	t.Run("Versions", func(t *testing.T) { testVersions(t, factory) })
	t.Run("ALPN", func(t *testing.T) { testALPN(t, factory) })
	t.Run("Pinning", func(t *testing.T) { testPinning(t, factory) })
	t.Run("MutualTLS", func(t *testing.T) { testMTLS(t, factory) })
	t.Run("ReuseAndOwnership", func(t *testing.T) { testReuse(t, factory); testCancellation(t, factory) })
	t.Run("Replay", func(t *testing.T) { testReplay(t, factory); testH2Replay(t, factory) })
	t.Run("Timeouts", func(t *testing.T) { testTimeouts(t, factory) })
}

func server(t *testing.T, h http.Handler, cfg *tls.Config, h2 bool) *httptest.Server {
	t.Helper()

	if h == nil {
		h = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	}

	s := httptest.NewUnstartedServer(h)
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.TLS = cfg
	s.EnableHTTP2 = h2
	s.StartTLS()
	t.Cleanup(s.Close)

	return s
}

func environment(s *httptest.Server) client.DriverEnv {
	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())

	return client.DriverEnv{
		TLS: &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12},
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, s.Listener.Addr().String())
		},
		TLSHandshakeTimeout: shortTimeout, ResponseHeaderTimeout: shortTimeout,
		MaxIdleConns: testIdleConns, MaxIdleConnsPerHost: testIdleConns, MaxConnsPerHost: 1, IdleConnTimeout: time.Minute,
	}
}

func transport(t *testing.T, d client.TransportDriver, env client.DriverEnv, refusalOK bool) http.RoundTripper {
	t.Helper()

	tr, err := d.NewTransport(env)
	if refusalOK && err != nil {
		t.Logf("configuration refused: %v", err)
		return nil
	}

	require.NoError(t, err)
	require.NotNil(t, tr)
	closer, ok := tr.(interface{ CloseIdleConnections() })
	require.True(t, ok, "driver must release idle connections")
	t.Cleanup(closer.CloseIdleConnections)

	return tr
}

type responseSummary struct{ StatusCode, ProtoMajor int }

func roundTrip(t *testing.T, tr http.RoundTripper, method string, body io.Reader) (*responseSummary, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, "https://"+authority+"/", body)
	require.NoError(t, err)

	resp, err := tr.RoundTrip(req)
	if resp != nil {
		_, readErr := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		if err == nil {
			err = readErr
		}
	}

	if resp == nil {
		return nil, err
	}

	return &responseSummary{StatusCode: resp.StatusCode, ProtoMajor: resp.ProtoMajor}, err
}

func testDial(t *testing.T, f func(*testing.T) client.TransportDriver) {
	for _, mode := range []string{"initial", "reconnect", "fallback", modeResumption, "proxy"} {
		t.Run(mode, func(t *testing.T) {
			var (
				hits    atomic.Int32
				resumed atomic.Bool
			)

			s := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				resumed.Store(r.TLS.DidResume)

				_, _ = io.WriteString(w, "ok")
			}), nil, false)

			env := environment(s)
			if mode == modeResumption {
				env.TLS.ClientSessionCache = tls.NewLRUClientSessionCache(sessionCacheSize)
			}

			if mode == "fallback" {
				env.TLS.NextProtos = []string{"h2", protocolHTTP1}
			}

			if mode == "proxy" {
				env.Dial = proxyDial(t, s)
			}

			sentinel := errors.New("injected dial refusal")

			var (
				refuse atomic.Bool
				dials  atomic.Int32
			)

			dial := env.Dial
			env.Dial = func(ctx context.Context, n, a string) (net.Conn, error) {
				dials.Add(1)

				if refuse.Load() {
					return nil, sentinel
				}

				return dial(ctx, n, a)
			}

			tr := transport(t, f(t), env, mode == modeResumption)
			if tr == nil {
				return
			}

			_, err := roundTrip(t, tr, "GET", nil)
			require.NoError(t, err)
			require.Positive(t, dials.Load(), "synthetic authority must use env.Dial")

			if mode == modeResumption {
				closeTransport(t, tr)
				_, err = roundTrip(t, tr, "GET", nil)
				require.NoError(t, err)
				require.True(t, resumed.Load(), "accepted session cache must allow resumption")
				require.EqualValues(t, twoConnections, dials.Load())
			}

			if mode == "initial" {
				tr = transport(t, f(t), env, false)
			} else {
				closeTransport(t, tr)
				s.CloseClientConnections()
			}

			before := hits.Load()

			refuse.Store(true)

			_, err = roundTrip(t, tr, "GET", nil)
			require.ErrorIs(t, err, sentinel)
			require.Equal(t, before, hits.Load())
		})
	}
}

func testVerification(t *testing.T, f func(*testing.T) client.TransportDriver) {
	s := server(t, nil, nil, false)
	for _, name := range []string{"untrusted", "wrong name", modeInsecure} {
		t.Run(name, func(t *testing.T) {
			env := environment(s)

			switch name {
			case "untrusted":
				env.TLS.RootCAs = x509.NewCertPool()
			case "wrong name":
				env.TLS.ServerName = "wrong.invalid"
			case modeInsecure:
				env.TLS.InsecureSkipVerify = true
			}

			if name == modeInsecure {
				require.Panics(t, func() {
					client.New(client.WithTransport(&http.Transport{TLSClientConfig: env.TLS}), client.WithTransportDriver(f(t)))
				})

				return
			}

			tr := transport(t, f(t), env, false)
			_, err := roundTrip(t, tr, "GET", nil)
			require.Error(t, err)
		})
	}
}

func testVersions(t *testing.T, f func(*testing.T) client.TransportDriver) {
	for _, tc := range []struct {
		name                                       string
		serverMin, serverMax, clientMin, clientMax uint16
	}{
		{"minimum", tls.VersionTLS12, tls.VersionTLS12, tls.VersionTLS13, 0},
		{"maximum", tls.VersionTLS13, tls.VersionTLS13, tls.VersionTLS12, tls.VersionTLS12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := server(t, nil, &tls.Config{MinVersion: tc.serverMin, MaxVersion: tc.serverMax}, false)
			env := environment(s)
			env.TLS.MinVersion = tc.clientMin
			env.TLS.MaxVersion = tc.clientMax

			tr := transport(t, f(t), env, true)
			if tr == nil {
				return
			}

			_, err := roundTrip(t, tr, "GET", nil)
			require.Error(t, err)
		})
	}
}

func testALPN(t *testing.T, f func(*testing.T) client.TransportDriver) {
	for _, tc := range []struct {
		name                   string
		serverALPN, clientALPN []string
		want                   int
	}{
		{"h1 restricted", []string{"h2", protocolHTTP1}, []string{protocolHTTP1}, 1},
		{"h2 only", []string{"h2"}, []string{"h2"}, 2},
		{"h1 only", []string{protocolHTTP1}, []string{"h2", protocolHTTP1}, 1},
		{"both", []string{"h2", protocolHTTP1}, []string{"h2", protocolHTTP1}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			offered := make(chan []string, 8)
			cfg := &tls.Config{NextProtos: tc.serverALPN, MinVersion: tls.VersionTLS12,
				GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
					offered <- append([]string(nil), hello.SupportedProtos...)
					return nil, nil //nolint:nilnil // TLS callback keeps the server configuration.
				}}
			s := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Protocol", r.Proto)
				_, _ = io.WriteString(w, "ok")
			}), cfg, true)
			env := environment(s)
			env.TLS.NextProtos = tc.clientALPN
			tr := transport(t, f(t), env, false)
			resp, err := roundTrip(t, tr, "GET", nil)
			require.NoError(t, err)
			require.Equal(t, tc.want, resp.ProtoMajor)

			if tc.name == "h1 restricted" {
				require.NotContains(t, <-offered, "h2")
			}
		})
	}
}

func testPinning(t *testing.T, f func(*testing.T) client.TransportDriver) {
	s := server(t, nil, nil, false)
	for _, name := range []string{"peer", "connection"} {
		t.Run(name, func(t *testing.T) {
			env := environment(s)
			sentinel := errors.New("pin rejected")

			var calls atomic.Int32

			if name == "peer" {
				env.TLS.VerifyPeerCertificate = func([][]byte, [][]*x509.Certificate) error { calls.Add(1); return sentinel }
			} else {
				env.TLS.VerifyConnection = func(tls.ConnectionState) error { calls.Add(1); return sentinel }
			}

			tr := transport(t, f(t), env, true)
			if tr == nil {
				return
			}

			_, err := roundTrip(t, tr, "GET", nil)
			require.ErrorIs(t, err, sentinel)
			require.Positive(t, calls.Load())
		})
	}
}

func testMTLS(t *testing.T, f func(*testing.T) client.TransportDriver) {
	for _, callback := range []bool{false, true} {
		t.Run(map[bool]string{false: "certificates", true: "callback"}[callback], func(t *testing.T) {
			var seen atomic.Bool

			s := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen.Store(len(r.TLS.PeerCertificates) > 0)

				_, _ = io.WriteString(w, "ok")
			}),
				&tls.Config{ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS12}, false)
			env := environment(s)

			cert := s.TLS.Certificates[0]
			if callback {
				env.TLS.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &cert, nil }
			} else {
				env.TLS.Certificates = []tls.Certificate{cert}
			}

			tr := transport(t, f(t), env, true)
			if tr == nil {
				return
			}

			_, err := roundTrip(t, tr, "GET", nil)
			require.NoError(t, err)
			require.True(t, seen.Load())
		})
	}
}

func testReuse(t *testing.T, f func(*testing.T) client.TransportDriver) {
	for _, h2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "h1", true: "h2"}[h2], func(t *testing.T) {
			var handshakes atomic.Int32

			s := server(t, nil, &tls.Config{MinVersion: tls.VersionTLS12, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
				handshakes.Add(1)
				return nil, nil //nolint:nilnil // TLS callback keeps the server configuration.
			}}, h2)

			env := environment(s)
			if h2 {
				env.TLS.NextProtos = []string{"h2"}
			}

			tr := transport(t, f(t), env, false)
			for range 5 {
				_, err := roundTrip(t, tr, "GET", nil)
				require.NoError(t, err)
			}

			require.EqualValues(t, 1, handshakes.Load())

			var wg sync.WaitGroup
			for range 5 {
				wg.Go(func() { _, err := roundTrip(t, tr, "GET", nil); require.NoError(t, err) })
			}

			wg.Wait()
			require.EqualValues(t, 1, handshakes.Load(), "MaxConnsPerHost=1 must be honored")
			closeTransport(t, tr)
			_, err := roundTrip(t, tr, "GET", nil)
			require.NoError(t, err)
			require.EqualValues(t, twoConnections, handshakes.Load())
		})
	}
}

func testCancellation(t *testing.T, f func(*testing.T) client.TransportDriver) {
	t.Run("cancel handshake", func(t *testing.T) {
		entered, closed := make(chan struct{}), make(chan struct{})
		env := client.DriverEnv{TLSHandshakeTimeout: testTimeout}
		env.Dial = func(context.Context, string, string) (net.Conn, error) {
			a, b := net.Pipe()

			go func() {
				defer close(closed)
				defer func() { _ = b.Close() }()

				buf := make([]byte, 1)
				if _, err := b.Read(buf); err == nil {
					close(entered)
				}

				_, _ = io.Copy(io.Discard, b)
			}()

			return a, nil
		}
		tr := transport(t, f(t), env, false)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, "GET", "https://"+authority, nil)
		require.NoError(t, err)

		done := make(chan error, 1)

		go func() {
			resp, err := tr.RoundTrip(req)
			if resp != nil {
				_ = resp.Body.Close()
			}

			done <- err
		}()

		select {
		case <-entered:
		case <-time.After(testTimeout):
			t.Fatal("handshake not started")
		}

		cancel()

		select {
		case err := <-done:
			require.Error(t, err)
		case <-time.After(testTimeout):
			t.Fatal("request did not cancel")
		}

		closeTransport(t, tr)

		select {
		case <-closed:
		case <-time.After(testTimeout):
			t.Fatal("canceled handshake leaked connection")
		}
	})
	t.Run("cancel dial", func(t *testing.T) {
		entered, exited := make(chan struct{}), make(chan struct{})
		env := client.DriverEnv{Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			close(entered)
			<-ctx.Done()
			close(exited)

			return nil, ctx.Err()
		}}
		tr := transport(t, f(t), env, false)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, "GET", "https://"+authority, nil)
		require.NoError(t, err)

		done := make(chan error, 1)

		go func() {
			resp, err := tr.RoundTrip(req)
			if resp != nil {
				_ = resp.Body.Close()
			}

			done <- err
		}()

		select {
		case <-entered:
		case <-time.After(testTimeout):
			t.Fatal("dial not started")
		}

		cancel()

		select {
		case err := <-done:
			require.Error(t, err)
		case <-time.After(testTimeout):
			t.Fatal("request did not cancel")
		}
		// net/http detaches dials for reuse; CloseIdleConnections must retire them.
		closeTransport(t, tr)

		select {
		case <-exited:
		case <-time.After(testTimeout):
			t.Fatal("dial leaked after cancellation and idle close")
		}
	})
}

func testReplay(t *testing.T, f func(*testing.T) client.TransportDriver) {
	for _, wrapped := range []bool{false, true} {
		for _, rewindable := range []bool{false, true} {
			name := map[bool]string{false: "transport", true: modeWithoutRetry}[wrapped] +
				map[bool]string{false: "/stream", true: "/rewindable"}[rewindable]
			t.Run(name, func(t *testing.T) {
				var hits atomic.Int32

				s := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)

					hits.Add(1)

					hijacker, ok := w.(http.Hijacker)
					if !ok {
						return
					}

					conn, _, err := hijacker.Hijack()
					if err == nil {
						_ = conn.Close()
					}
				}), nil, false)
				env := environment(s)

				var tr http.RoundTripper

				if wrapped {
					c := client.New(client.WithoutSSRFProtection(), client.WithoutProxy(), client.WithoutRetry(), client.WithTransportDriver(f(t)),
						client.WithTransport(&http.Transport{DialContext: env.Dial, TLSClientConfig: env.TLS}))
					t.Cleanup(c.CloseIdleConnections)
					tr = c.Transport
				} else {
					tr = transport(t, f(t), env, false)
				}

				var body io.Reader = strings.NewReader(payload)
				if !rewindable {
					body = io.LimitReader(body, int64(len(payload)))
				}

				_, err := roundTrip(t, tr, "POST", body)
				require.Error(t, err)
				require.EqualValues(t, 1, hits.Load())
			})
		}
	}
}

func testTimeouts(t *testing.T, f func(*testing.T) client.TransportDriver) {
	t.Run("handshake", func(t *testing.T) {
		env := client.DriverEnv{TLSHandshakeTimeout: shortTimeout}
		closed := make(chan struct{})
		env.Dial = func(context.Context, string, string) (net.Conn, error) {
			a, b := net.Pipe()

			go func() { defer close(closed); defer func() { _ = b.Close() }(); _, _ = io.Copy(io.Discard, b) }()

			return a, nil
		}
		tr := transport(t, f(t), env, false)
		start := time.Now()
		_, err := roundTrip(t, tr, "GET", nil)
		require.Error(t, err)
		require.Less(t, time.Since(start), testTimeout/timeoutMargin)

		select {
		case <-closed:
		case <-time.After(testTimeout):
			t.Fatal("handshake connection leaked")
		}
	})
	t.Run("response header", func(t *testing.T) {
		s := server(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }), nil, false)
		tr := transport(t, f(t), environment(s), false)
		start := time.Now()
		_, err := roundTrip(t, tr, "GET", nil)
		require.Error(t, err)
		require.Less(t, time.Since(start), testTimeout/timeoutMargin)
	})
}

func closeTransport(t *testing.T, tr http.RoundTripper) {
	t.Helper()

	closer, ok := tr.(interface{ CloseIdleConnections() })
	require.True(t, ok)
	closer.CloseIdleConnections()
}

const (
	testIdleConns    = 10
	sessionCacheSize = 4
	twoConnections   = 2
	timeoutMargin    = 2
	payload          = "payload"
	modeResumption   = "resumption"
	protocolHTTP1    = "http/1.1"
	modeInsecure     = "insecure"
	modeWithoutRetry = "WithoutRetry"
)
