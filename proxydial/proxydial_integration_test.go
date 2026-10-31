//go:build integration

// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package proxydial_test

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/proxydial"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

// itProxy is the bookkeeping shared by the loopback proxies below: the
// destinations they were asked for, the connections they accepted, and the
// open connections closed on cleanup.
type itProxy struct {
	url      *url.URL
	accepted atomic.Int32

	mu    sync.Mutex
	dsts  []string
	conns map[net.Conn]struct{}
}

func (p *itProxy) record(dst string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dsts = append(p.dsts, dst)
}

func (p *itProxy) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.dsts)
}

// serveLoopback accepts connections on 127.0.0.1 and hands each to serve.
func serveLoopback(tb testing.TB, scheme string, serve func(p *itProxy, conn net.Conn)) *itProxy {
	tb.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(tb, err)
	p := &itProxy{url: &url.URL{Scheme: scheme, Host: ln.Addr().String()}, conns: make(map[net.Conn]struct{})}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			p.accepted.Add(1)
			p.mu.Lock()
			p.conns[conn] = struct{}{}
			p.mu.Unlock()
			wg.Go(func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				serve(p, conn)
			})
		}
	})
	tb.Cleanup(func() {
		_ = ln.Close()
		p.mu.Lock()
		for conn := range p.conns {
			_ = conn.Close()
		}
		p.mu.Unlock()
		wg.Wait()
	})
	return p
}

// relay copies between the client and the upstream until either side ends.
func relay(client net.Conn, clientReader io.Reader, upstream net.Conn) {
	defer func() { _ = upstream.Close() }()
	_ = upstream.SetDeadline(time.Now().Add(10 * time.Second))
	go func() {
		_, _ = io.Copy(upstream, clientReader)
		_ = upstream.Close()
	}()
	_, _ = io.Copy(client, upstream)
}

// startITConnectProxy starts an HTTP CONNECT proxy. With a user set it
// requires matching Basic proxy credentials and answers 407 otherwise.
func startITConnectProxy(tb testing.TB, user, pass string) *itProxy {
	tb.Helper()

	return serveLoopback(tb, "http", func(p *itProxy, conn net.Conn) {
		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil || req.Method != http.MethodConnect {
			return
		}
		if user != "" {
			want := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
			if req.Header.Get("Proxy-Authorization") != want {
				_, _ = io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n")
				return
			}
		}
		p.record(req.Host)
		upstream, err := net.DialTimeout("tcp", req.Host, 5*time.Second)
		if err != nil {
			_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
			return
		}
		if _, err = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			_ = upstream.Close()
			return
		}
		relay(conn, br, upstream)
	})
}

// startITSOCKS5 starts a relaying SOCKS5 server (RFC 1928). With a user set
// it requires username/password authentication (RFC 1929).
func startITSOCKS5(tb testing.TB, user, pass string) *itProxy {
	tb.Helper()

	return serveLoopback(tb, "socks5", func(p *itProxy, conn net.Conn) {
		if !socks5Handshake(conn, user, pass) {
			return
		}
		dst, ok := socks5ReadConnect(conn)
		if !ok {
			return
		}
		p.record(dst)
		upstream, err := net.DialTimeout("tcp", dst, 5*time.Second)
		if err != nil {
			_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0}) // connection refused
			return
		}
		if _, err = conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
			_ = upstream.Close()
			return
		}
		relay(conn, conn, upstream)
	})
}

func socks5Handshake(conn net.Conn, user, pass string) bool {
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil || head[0] != 5 {
		return false
	}
	methods := make([]byte, head[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return false
	}
	method := byte(0)
	if user != "" {
		method = 2
	}
	if !slices.Contains(methods, method) {
		_, _ = conn.Write([]byte{5, 0xff})
		return false
	}
	if _, err := conn.Write([]byte{5, method}); err != nil {
		return false
	}
	if method == 0 {
		return true
	}
	ver := make([]byte, 2)
	if _, err := io.ReadFull(conn, ver); err != nil {
		return false
	}
	gotUser := make([]byte, ver[1])
	if _, err := io.ReadFull(conn, gotUser); err != nil {
		return false
	}
	plen := make([]byte, 1)
	if _, err := io.ReadFull(conn, plen); err != nil {
		return false
	}
	gotPass := make([]byte, plen[0])
	if _, err := io.ReadFull(conn, gotPass); err != nil {
		return false
	}
	status := byte(0)
	if string(gotUser) != user || string(gotPass) != pass {
		status = 1
	}
	_, err := conn.Write([]byte{1, status})
	return err == nil && status == 0
}

// socks5ReadConnect reads a CONNECT request and returns its destination as
// "host:port".
func socks5ReadConnect(conn net.Conn) (string, bool) {
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil || req[1] != 1 {
		return "", false
	}
	var host string
	switch req[3] {
	case 1, 4:
		ip := make([]byte, map[byte]int{1: 4, 4: 16}[req[3]])
		if _, err := io.ReadFull(conn, ip); err != nil {
			return "", false
		}
		a, _ := netip.AddrFromSlice(ip)
		host = a.String()
	case 3:
		n := make([]byte, 1)
		if _, err := io.ReadFull(conn, n); err != nil {
			return "", false
		}
		name := make([]byte, n[0])
		if _, err := io.ReadFull(conn, name); err != nil {
			return "", false
		}
		host = string(name)
	default:
		return "", false
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return "", false
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port)))), true
}

// itTransport returns an HTTP transport that dials every connection through
// dial, verifies TLS targets with the roots of tlsTarget (when set), and
// never reuses connections, so each request opens its own tunnel.
func itTransport(dial proxydial.DialContextFunc, tlsTarget *httptest.Server) *http.Transport {
	t := &http.Transport{DialContext: dial, DisableKeepAlives: true, ForceAttemptHTTP2: false}
	if tlsTarget != nil {
		t.TLSClientConfig = tlsTarget.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	}
	return t
}

func itFetch(tb testing.TB, t *http.Transport, rawURL string) (string, error) {
	tb.Helper()

	req, err := http.NewRequestWithContext(tb.Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Transport: t}).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

func hostEcho() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Host+r.URL.Path)
	})
}

// TestIntegration_HTTPTransportThroughProxies drives net/http transports over
// FromURL dialers for every proxy kind, to plain and TLS targets.
func TestIntegration_HTTPTransportThroughProxies(t *testing.T) {
	t.Parallel()

	plain := httptest.NewServer(hostEcho())
	t.Cleanup(plain.Close)
	secure := httptest.NewTLSServer(hostEcho())
	t.Cleanup(secure.Close)
	plainHost := plain.Listener.Addr().String()
	secureHost := secure.Listener.Addr().String()

	cases := []struct {
		name  string
		start func(tb testing.TB) *itProxy
		creds *url.Userinfo
	}{
		{"connect", func(tb testing.TB) *itProxy { return startITConnectProxy(tb, "", "") }, nil},
		{"connect_basic_auth", func(tb testing.TB) *itProxy { return startITConnectProxy(tb, "svc", "p@ss:word") }, url.UserPassword("svc", "p@ss:word")},
		{"socks5", func(tb testing.TB) *itProxy { return startITSOCKS5(tb, "", "") }, nil},
		{"socks5_userpass", func(tb testing.TB) *itProxy { return startITSOCKS5(tb, "svc", "s3cret") }, url.UserPassword("svc", "s3cret")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proxy := tc.start(t)
			proxyURL := *proxy.url
			proxyURL.User = tc.creds
			dial, err := proxydial.FromURL(&proxyURL)
			require.NoError(t, err)

			body, err := itFetch(t, itTransport(dial, nil), plain.URL+"/p")
			require.NoError(t, err)
			require.Equal(t, plainHost+"/p", body)

			body, err = itFetch(t, itTransport(dial, secure), secure.URL+"/s")
			require.NoError(t, err)
			require.Equal(t, secureHost+"/s", body, "TLS runs end to end through the tunnel")

			require.Equal(t, []string{plainHost, secureHost}, proxy.seen())
		})
	}
}

func TestIntegration_ProxyCredentialsRejected(t *testing.T) {
	t.Parallel()

	target := httptest.NewServer(hostEcho())
	t.Cleanup(target.Close)

	cases := []struct {
		name  string
		proxy func(tb testing.TB) *itProxy
		creds *url.Userinfo
	}{
		{"connect_missing", func(tb testing.TB) *itProxy { return startITConnectProxy(tb, "svc", "right") }, nil},
		{"connect_wrong", func(tb testing.TB) *itProxy { return startITConnectProxy(tb, "svc", "right") }, url.UserPassword("svc", "wrong")},
		{"socks5_missing", func(tb testing.TB) *itProxy { return startITSOCKS5(tb, "svc", "right") }, nil},
		{"socks5_wrong", func(tb testing.TB) *itProxy { return startITSOCKS5(tb, "svc", "right") }, url.UserPassword("svc", "wrong")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proxy := tc.proxy(t)
			proxyURL := *proxy.url
			proxyURL.User = tc.creds
			dial, err := proxydial.FromURL(&proxyURL)
			require.NoError(t, err)

			_, err = itFetch(t, itTransport(dial, nil), target.URL)
			require.Error(t, err)
			require.Empty(t, proxy.seen(), "no tunnel may be opened without valid credentials")
		})
	}
}

// TestIntegration_SSRFGuardOverRealProxies resolves destinations locally,
// lets only an explicitly permitted loopback target through and asks the
// proxy for the checked IP, never the host name.
func TestIntegration_SSRFGuardOverRealProxies(t *testing.T) {
	t.Parallel()

	permitted := httptest.NewServer(hostEcho())
	t.Cleanup(permitted.Close)
	forbidden := httptest.NewServer(hostEcho())
	t.Cleanup(forbidden.Close)
	permittedAddr := permitted.Listener.Addr().(*net.TCPAddr).AddrPort()
	forbiddenPort := strconv.Itoa(forbidden.Listener.Addr().(*net.TCPAddr).Port)
	permittedPort := strconv.Itoa(int(permittedAddr.Port()))

	policy, err := httpclient.NewEgressPolicy(
		[]httpclient.EgressRule{{CIDR: netip.PrefixFrom(permittedAddr.Addr(), 32), Port: permittedAddr.Port()}}, nil)
	require.NoError(t, err)
	resolver := &fakeResolver{answers: map[string][][]netip.Addr{
		"app.test":      {addrs("127.0.0.1")},
		"metadata.test": {addrs("169.254.169.254")},
	}}

	starters := map[string]func(tb testing.TB) *itProxy{
		"connect": func(tb testing.TB) *itProxy { return startITConnectProxy(tb, "", "") },
		"socks5":  func(tb testing.TB) *itProxy { return startITSOCKS5(tb, "", "") },
	}
	for name, start := range starters {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			proxy := start(t)
			dial, err := proxydial.FromURL(proxy.url, proxydial.WithSSRFGuard(policy.Check), proxydial.WithResolver(resolver))
			require.NoError(t, err)
			tr := itTransport(dial, nil)

			body, err := itFetch(t, tr, "http://app.test:"+permittedPort+"/ok")
			require.NoError(t, err)
			require.Equal(t, "app.test:"+permittedPort+"/ok", body, "the Host header keeps the name")
			require.Equal(t, []string{"127.0.0.1:" + permittedPort}, proxy.seen(), "the proxy is asked for the checked IP")

			for _, refused := range []string{
				"http://127.0.0.1:" + forbiddenPort + "/",
				"http://app.test:" + forbiddenPort + "/",
				"http://metadata.test/latest",
			} {
				_, err = itFetch(t, tr, refused)
				require.ErrorIs(t, err, httpclient.ErrSSRFBlocked, refused)
			}
			require.Equal(t, int32(1), proxy.accepted.Load(), "refused destinations never reach the proxy")
		})
	}
}

// TestIntegration_ConcurrentTunnels opens many tunnels at once through each
// proxy kind.
func TestIntegration_ConcurrentTunnels(t *testing.T) {
	t.Parallel()

	target := httptest.NewServer(hostEcho())
	t.Cleanup(target.Close)
	const n = 16

	starters := map[string]func(tb testing.TB) *itProxy{
		"connect": func(tb testing.TB) *itProxy { return startITConnectProxy(tb, "", "") },
		"socks5":  func(tb testing.TB) *itProxy { return startITSOCKS5(tb, "", "") },
	}
	for name, start := range starters {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			proxy := start(t)
			dial, err := proxydial.FromURL(proxy.url)
			require.NoError(t, err)
			tr := itTransport(dial, nil)

			var wg sync.WaitGroup
			errs := make(chan error, n)
			for i := range n {
				wg.Go(func() {
					body, ferr := itFetch(t, tr, target.URL+"/"+strconv.Itoa(i))
					if ferr == nil && body != target.Listener.Addr().String()+"/"+strconv.Itoa(i) {
						ferr = io.ErrUnexpectedEOF
					}
					errs <- ferr
				})
			}
			wg.Wait()
			close(errs)
			for ferr := range errs {
				require.NoError(t, ferr)
			}
			require.Equal(t, int32(n), proxy.accepted.Load())
		})
	}
}
