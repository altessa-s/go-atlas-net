// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package proxydial_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/proxydial"
)

// tlsConnectProxy is an https:// CONNECT proxy served by an httptest TLS
// server: every CONNECT is hijacked and handed to handle.
type tlsConnectProxy struct {
	url      *url.URL
	roots    *x509.CertPool
	connects atomic.Int32
}

func startTLSConnectProxy(tb testing.TB, handle func(conn net.Conn, rw *bufio.ReadWriter)) *tlsConnectProxy {
	tb.Helper()

	p := &tlsConnectProxy{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		p.connects.Add(1)
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		handle(conn, rw)
	}))
	tb.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(tb, err)
	p.url = u
	p.roots = x509.NewCertPool()
	p.roots.AddCert(srv.Certificate())
	return p
}

func TestHTTPConnect_HTTPSProxy(t *testing.T) {
	t.Parallel()

	const established = "HTTP/1.1 200 Connection established\r\n\r\n"

	t.Run("client_first_returns_tls_conn", func(t *testing.T) {
		t.Parallel()

		proxy := startTLSConnectProxy(t, func(conn net.Conn, rw *bufio.ReadWriter) {
			if _, err := io.WriteString(conn, established); err != nil {
				return
			}
			line, err := rw.ReadString('\n')
			if err != nil {
				return
			}
			_, _ = io.WriteString(conn, "pong:"+line)
		})

		conn, err := proxydial.HTTPConnect(t.Context(), &net.Dialer{}, proxy.url, &tls.Config{RootCAs: proxy.roots}, "api.example:443")
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
		require.IsType(t, &tls.Conn{}, conn, "no buffered bytes: the TLS connection to the proxy is the tunnel")

		_, err = io.WriteString(conn, "ping\n")
		require.NoError(t, err)
		got, err := bufio.NewReader(conn).ReadString('\n')
		require.NoError(t, err)
		require.Equal(t, "pong:ping\n", got)
	})

	t.Run("server_first_keeps_close_write", func(t *testing.T) {
		t.Parallel()

		const greeting = "220 smtp.example ESMTP\r\n"
		proxy := startTLSConnectProxy(t, func(conn net.Conn, rw *bufio.ReadWriter) {
			if _, err := io.WriteString(conn, established+greeting); err != nil {
				return
			}
			if _, err := io.Copy(io.Discard, rw); err != nil {
				return
			}
			_, _ = io.WriteString(conn, "bye")
		})

		conn, err := proxydial.HTTPConnect(t.Context(), &net.Dialer{}, proxy.url, &tls.Config{RootCAs: proxy.roots}, "smtp.example:25")
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))

		got := make([]byte, len(greeting))
		_, err = io.ReadFull(conn, got)
		require.NoError(t, err)
		require.Equal(t, greeting, string(got), "the greeting sent with the 200 must reach the caller")

		_, ok := conn.(interface{ CloseRead() error })
		require.False(t, ok, "a TLS tunnel has no CloseRead")
		cw, ok := conn.(interface{ CloseWrite() error })
		require.True(t, ok, "CloseWrite of the TLS connection must survive the wrapper")
		require.NoError(t, cw.CloseWrite())

		rest, err := io.ReadAll(conn)
		require.NoError(t, err)
		require.Equal(t, "bye", string(rest), "the proxy must see EOF after CloseWrite and still answer")
	})
}

func TestHTTPConnect_HTTPSProxyUntrustedCertificate(t *testing.T) {
	t.Parallel()

	proxy := startTLSConnectProxy(t, func(net.Conn, *bufio.ReadWriter) {})

	_, err := proxydial.HTTPConnect(t.Context(), &net.Dialer{}, proxy.url, nil, "api.example:443")
	var verr *tls.CertificateVerificationError
	require.ErrorAs(t, err, &verr, "the proxy certificate is not signed by a system root")
	require.Zero(t, proxy.connects.Load(), "no CONNECT may be sent over an unverified connection")
}

func TestFromURL_ProxyTLSConfig(t *testing.T) {
	t.Parallel()

	proxy := startTLSConnectProxy(t, func(conn net.Conn, _ *bufio.ReadWriter) {
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
	})
	userCfg := &tls.Config{RootCAs: proxy.roots}

	dial, err := proxydial.FromURL(proxy.url, proxydial.WithProxyTLSConfig(userCfg), proxydial.WithProxyTLSConfig(nil))
	require.NoError(t, err)
	conn, err := dial(t.Context(), "tcp", "api.example:443")
	require.NoError(t, err)
	_ = conn.Close()

	require.Equal(t, int32(1), proxy.connects.Load())
	require.Empty(t, userCfg.ServerName, "the caller's TLS config must not be mutated")
	require.Zero(t, userCfg.MinVersion, "the caller's TLS config must not be mutated")
}

func TestHTTPConnect_ProxyUnreachable(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			t.Parallel()

			_, err := proxydial.HTTPConnect(t.Context(), &net.Dialer{}, &url.URL{Scheme: scheme, Host: addr}, nil, "api.example:443")
			require.ErrorIs(t, err, syscall.ECONNREFUSED)
		})
	}
}

// TestHTTPConnect_BufferedTunnelCloseRead verifies that shutting down the
// read side of a buffered tunnel leaves its write side usable.
func TestHTTPConnect_BufferedTunnelCloseRead(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	received := make(chan string, 1)
	go func() {
		defer close(received)
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		br := bufio.NewReader(conn)
		if _, rerr := http.ReadRequest(br); rerr != nil {
			return
		}
		if _, werr := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\nSSH-2.0-test\r\n"); werr != nil {
			return
		}
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			return
		}
		received <- line
	}()

	conn, err := proxydial.HTTPConnect(t.Context(), &net.Dialer{}, &url.URL{Scheme: "http", Host: ln.Addr().String()}, nil, "ssh.example:22")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))

	cr, ok := conn.(interface{ CloseRead() error })
	require.True(t, ok, "a buffered TCP tunnel keeps CloseRead")
	require.NoError(t, cr.CloseRead())

	_, err = io.WriteString(conn, "SSH-2.0-client\r\n")
	require.NoError(t, err)
	select {
	case line := <-received:
		require.Equal(t, "SSH-2.0-client\r\n", line)
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy never received the client line")
	}
}

// TestSSRFGuard_CancelStopsTryingAddresses verifies that a dial canceled while
// the first approved address is being tried does not go on to the next one.
func TestSSRFGuard_CancelStopsTryingAddresses(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	arrived := make(chan struct{})
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		br := bufio.NewReader(conn)
		if _, rerr := http.ReadRequest(br); rerr != nil {
			return
		}
		close(arrived)
		_, _ = br.ReadByte() // stall until the client gives up
	}()

	resolver := &fakeResolver{answers: map[string][][]netip.Addr{"multi.example": {addrs("93.184.216.34", "93.184.216.35")}}}
	dial := guardedDialer(t, &url.URL{Scheme: "http", Host: ln.Addr().String()}, resolver, defaultGuard(t))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		conn, derr := dial(ctx, "tcp", "multi.example:443")
		if conn != nil {
			_ = conn.Close()
		}
		result <- derr
	}()

	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("CONNECT never reached the proxy")
	}
	cancel()

	select {
	case err = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("the dial did not return after cancellation")
	}
	require.ErrorIs(t, err, context.Canceled)
	joined, ok := err.(interface{ Unwrap() []error })
	require.True(t, ok, "the dial joins the errors of the addresses it tried")
	require.Len(t, joined.Unwrap(), 1, "the second approved address must not be tried after cancellation")
}
