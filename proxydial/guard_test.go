// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package proxydial_test

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
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

// fakeResolver answers lookups from a script: the n-th lookup of a host gets
// the n-th answer (the last one repeats). It records every lookup.
type fakeResolver struct {
	answers map[string][][]netip.Addr
	err     error

	mu      sync.Mutex
	lookups []string // "network host"
}

func (r *fakeResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, l := range r.lookups {
		if l == network+" "+host {
			n++
		}
	}
	r.lookups = append(r.lookups, network+" "+host)
	if r.err != nil {
		return nil, r.err
	}
	answers := r.answers[host]
	if len(answers) == 0 {
		return nil, nil
	}
	return answers[min(n, len(answers)-1)], nil
}

func (r *fakeResolver) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.lookups)
}

func addrs(s ...string) []netip.Addr {
	out := make([]netip.Addr, 0, len(s))
	for _, a := range s {
		out = append(out, netip.MustParseAddr(a))
	}
	return out
}

// recordingProxy is an HTTP CONNECT proxy that records every CONNECT target.
// It refuses the targets listed in refuse with 502; otherwise it answers 200
// and, when backend is set, tunnels the connection to backend.
type recordingProxy struct {
	refuse  map[string]bool
	backend string

	accepted atomic.Int32
	mu       sync.Mutex
	targets  []string
}

func (p *recordingProxy) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.targets)
}

func startRecordingProxy(tb testing.TB, p *recordingProxy) *url.URL {
	tb.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(tb, err)
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			p.accepted.Add(1)
			wg.Go(func() { p.serve(conn) })
		}
	})
	tb.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	return &url.URL{Scheme: "http", Host: ln.Addr().String()}
}

func (p *recordingProxy) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	p.mu.Lock()
	p.targets = append(p.targets, req.Host)
	p.mu.Unlock()
	if p.refuse[req.Host] {
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	if p.backend == "" {
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
		return
	}
	upstream, err := net.Dial("tcp", p.backend)
	if err != nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer func() { _ = upstream.Close() }()
	_ = upstream.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	go func() { _, _ = io.Copy(upstream, br) }()
	_, _ = io.Copy(conn, upstream)
}

func defaultGuard(tb testing.TB) proxydial.AddrGuard {
	tb.Helper()
	p, err := httpclient.NewEgressPolicy(nil, []httpclient.EgressRule{{Host: "*.internal"}})
	require.NoError(tb, err)
	return p.Check
}

func guardedDialer(tb testing.TB, proxyURL *url.URL, resolver proxydial.Resolver, guard proxydial.AddrGuard) proxydial.DialContextFunc {
	tb.Helper()
	dial, err := proxydial.FromURL(proxyURL, proxydial.WithSSRFGuard(guard), proxydial.WithResolver(resolver))
	require.NoError(tb, err)
	return dial
}

// TestSSRFGuard_PinsCheckedIPAndKeepsHostName runs a real HTTPS request
// through the guarded dialer: the proxy is asked for the checked IP, while
// the Host header and the TLS server name stay the original host name (the
// test certificate is valid for example.com, not for the IP).
func TestSSRFGuard_PinsCheckedIPAndKeepsHostName(t *testing.T) {
	t.Parallel()

	var gotHost atomic.Value
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost.Store(r.Host)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	proxy := &recordingProxy{backend: srv.Listener.Addr().String()}
	proxyURL := startRecordingProxy(t, proxy)
	resolver := &fakeResolver{answers: map[string][][]netip.Addr{"example.com": {addrs("93.184.216.34")}}}

	transport := srv.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = guardedDialer(t, proxyURL, resolver, defaultGuard(t))
	client := &http.Client{Transport: transport}
	t.Cleanup(client.CloseIdleConnections)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/path", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.Equal(t, "example.com", gotHost.Load())
	require.Equal(t, []string{"93.184.216.34:443"}, proxy.seen())
	require.Equal(t, []string{"ip example.com"}, resolver.calls(), "one lookup: the checked address is the one sent to the proxy")
}

// TestSSRFGuard_RebindingNeedsANewLookup shows that every dial resolves
// once and connects to what it checked: a rebinding answer on a later dial
// is checked (and refused) on its own.
func TestSSRFGuard_RebindingNeedsANewLookup(t *testing.T) {
	t.Parallel()

	proxy := &recordingProxy{}
	proxyURL := startRecordingProxy(t, proxy)
	resolver := &fakeResolver{answers: map[string][][]netip.Addr{
		"rebind.example": {addrs("93.184.216.34"), addrs("169.254.169.254")},
	}}
	dial := guardedDialer(t, proxyURL, resolver, defaultGuard(t))

	conn, err := dial(t.Context(), "tcp", "rebind.example:80")
	require.NoError(t, err)
	_ = conn.Close()

	_, err = dial(t.Context(), "tcp", "rebind.example:80")
	require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)
	require.Equal(t, "169.254.169.254", httpclient.IsSSRFError(err).IP)

	require.Equal(t, []string{"93.184.216.34:80"}, proxy.seen())
	require.Len(t, resolver.calls(), 2)
}

func TestSSRFGuard_Refusals(t *testing.T) {
	t.Parallel()

	resolver := &fakeResolver{answers: map[string][][]netip.Addr{
		"metadata.example": {addrs("169.254.169.254")},
		"private.example":  {addrs("10.0.0.1")},
		"loopback.example": {addrs("127.0.0.1", "::1")},
		"ula.example":      {addrs("fd00::1")},
		"linklocal6.ex":    {addrs("fe80::1")},
		"mapped.example":   {addrs("::ffff:10.0.0.1")},
		"mixed.example":    {addrs("93.184.216.34", "192.168.1.1")},
		"db.internal":      {addrs("93.184.216.34")},
	}}
	tests := []struct {
		name string
		addr string
		ip   string // refused address reported in SSRFError.IP
	}{
		{"metadata by name", "metadata.example:80", "169.254.169.254"},
		{"rfc1918 by name", "private.example:443", "10.0.0.1"},
		{"loopback by name", "loopback.example:443", "127.0.0.1"},
		{"ipv6 ula by name", "ula.example:443", "fd00::1"},
		{"ipv6 link-local by name", "linklocal6.ex:443", "fe80::1"},
		{"ipv4-mapped private by name", "mapped.example:443", "10.0.0.1"},
		{"mixed public and private answers", "mixed.example:443", "192.168.1.1"},
		{"deny host rule", "db.internal:443", "93.184.216.34"},
		{"metadata literal", "169.254.169.254:80", "169.254.169.254"},
		{"mapped metadata literal", "[::ffff:169.254.169.254]:80", "169.254.169.254"},
		{"zoned link-local literal", "[fe80::1%eth0]:80", "fe80::1"},
		{"loopback v6 literal", "[::1]:80", "::1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proxy := &recordingProxy{}
			proxyURL := startRecordingProxy(t, proxy)
			dial := guardedDialer(t, proxyURL, resolver, defaultGuard(t))

			conn, err := dial(t.Context(), "tcp", tc.addr)
			require.Nil(t, conn)
			require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)
			ssrfErr := httpclient.IsSSRFError(err)
			require.NotNil(t, ssrfErr)
			require.Equal(t, tc.ip, ssrfErr.IP)
			require.Zero(t, proxy.accepted.Load(), "a refused destination never reaches the proxy")
		})
	}
}

func TestSSRFGuard_Literals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr   string
		target string
	}{
		{"93.184.216.34:443", "93.184.216.34:443"},
		{"[::ffff:93.184.216.34]:443", "93.184.216.34:443"},
		{"[2606:2800:220:1::1]:443", "[2606:2800:220:1::1]:443"},
	}
	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			t.Parallel()

			proxy := &recordingProxy{}
			proxyURL := startRecordingProxy(t, proxy)
			resolver := &fakeResolver{}
			conn, err := guardedDialer(t, proxyURL, resolver, defaultGuard(t))(t.Context(), "tcp", tc.addr)
			require.NoError(t, err)
			_ = conn.Close()
			require.Equal(t, []string{tc.target}, proxy.seen())
			require.Empty(t, resolver.calls(), "an IP literal is not resolved")
		})
	}
}

func TestSSRFGuard_ResolutionErrors(t *testing.T) {
	t.Parallel()

	t.Run("lookup error", func(t *testing.T) {
		t.Parallel()

		proxy := &recordingProxy{}
		proxyURL := startRecordingProxy(t, proxy)
		dnsErr := &net.DNSError{Err: "server misbehaving", Name: "x.example", IsTemporary: true}
		dial := guardedDialer(t, proxyURL, &fakeResolver{err: dnsErr}, defaultGuard(t))
		_, err := dial(t.Context(), "tcp", "x.example:443")
		got, ok := errors.AsType[*net.DNSError](err)
		require.True(t, ok)
		require.Same(t, dnsErr, got)
		require.Zero(t, proxy.accepted.Load())
	})

	t.Run("no addresses", func(t *testing.T) {
		t.Parallel()

		proxy := &recordingProxy{}
		proxyURL := startRecordingProxy(t, proxy)
		dial := guardedDialer(t, proxyURL, &fakeResolver{}, defaultGuard(t))
		_, err := dial(t.Context(), "tcp", "empty.example:443")
		got, ok := errors.AsType[*net.DNSError](err)
		require.True(t, ok)
		require.True(t, got.IsNotFound)
		require.Zero(t, proxy.accepted.Load())
	})

	t.Run("malformed address", func(t *testing.T) {
		t.Parallel()

		proxy := &recordingProxy{}
		proxyURL := startRecordingProxy(t, proxy)
		dial := guardedDialer(t, proxyURL, &fakeResolver{}, defaultGuard(t))
		for _, addr := range []string{"no-port", "host:99999", "host:http"} {
			_, err := dial(t.Context(), "tcp", addr)
			require.Error(t, err, addr)
		}
		require.Zero(t, proxy.accepted.Load())
	})
}

func TestSSRFGuard_TriesApprovedAddressesInOrder(t *testing.T) {
	t.Parallel()

	proxy := &recordingProxy{refuse: map[string]bool{"[2606:2800:220:1::1]:443": true}}
	proxyURL := startRecordingProxy(t, proxy)
	resolver := &fakeResolver{answers: map[string][][]netip.Addr{
		"dual.example": {addrs("2606:2800:220:1::1", "93.184.216.34")},
	}}
	conn, err := guardedDialer(t, proxyURL, resolver, defaultGuard(t))(t.Context(), "tcp", "dual.example:443")
	require.NoError(t, err)
	_ = conn.Close()
	require.Equal(t, []string{"[2606:2800:220:1::1]:443", "93.184.216.34:443"}, proxy.seen())

	t.Run("all fail", func(t *testing.T) {
		t.Parallel()

		proxy := &recordingProxy{refuse: map[string]bool{"93.184.216.34:443": true, "93.184.216.35:443": true}}
		proxyURL := startRecordingProxy(t, proxy)
		resolver := &fakeResolver{answers: map[string][][]netip.Addr{"down.example": {addrs("93.184.216.34", "93.184.216.35")}}}
		_, err := guardedDialer(t, proxyURL, resolver, defaultGuard(t))(t.Context(), "tcp", "down.example:443")
		require.ErrorContains(t, err, "93.184.216.34:443")
		require.ErrorContains(t, err, "93.184.216.35:443")
	})
}

func TestSSRFGuard_LookupNetworkFollowsDialNetwork(t *testing.T) {
	t.Parallel()

	proxy := &recordingProxy{}
	proxyURL := startRecordingProxy(t, proxy)
	resolver := &fakeResolver{answers: map[string][][]netip.Addr{"v.example": {addrs("93.184.216.34")}}}
	dial := guardedDialer(t, proxyURL, resolver, defaultGuard(t))
	for _, network := range []string{"tcp", "tcp4", "tcp6"} {
		conn, err := dial(t.Context(), network, "v.example:443")
		require.NoError(t, err)
		_ = conn.Close()
	}
	require.Equal(t, []string{"ip v.example", "ip4 v.example", "ip6 v.example"}, resolver.calls())
}

func TestSSRFGuard_GuardErrorIsWrapped(t *testing.T) {
	t.Parallel()

	errRefused := errors.New("caller sentinel")
	var seen []netip.AddrPort
	guard := func(host string, dst netip.AddrPort) error {
		require.Equal(t, "custom.example", host)
		seen = append(seen, dst)
		return errRefused
	}
	resolver := &fakeResolver{answers: map[string][][]netip.Addr{"custom.example": {addrs("::ffff:93.184.216.34")}}}
	proxy := &recordingProxy{}
	_, err := guardedDialer(t, startRecordingProxy(t, proxy), resolver, guard)(t.Context(), "tcp", "custom.example:8443")
	require.ErrorIs(t, err, errRefused)
	require.Equal(t, []netip.AddrPort{netip.MustParseAddrPort("93.184.216.34:8443")}, seen, "the guard sees the unmapped address")
	require.Zero(t, proxy.accepted.Load())
}

func TestSSRFGuard_WithoutGuardProxyResolves(t *testing.T) {
	t.Parallel()

	proxy := &recordingProxy{}
	proxyURL := startRecordingProxy(t, proxy)
	resolver := &fakeResolver{}
	dial, err := proxydial.FromURL(proxyURL, proxydial.WithResolver(resolver))
	require.NoError(t, err)
	conn, err := dial(t.Context(), "tcp", "example.com:443")
	require.NoError(t, err)
	_ = conn.Close()
	require.Equal(t, []string{"example.com:443"}, proxy.seen())
	require.Empty(t, resolver.calls())
}

// startSOCKS5 serves a minimal no-auth SOCKS5 server that records the
// CONNECT destination (as "atyp/ip:port") and answers success.
func startSOCKS5(tb testing.TB) (*url.URL, func() []string) {
	tb.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(tb, err)
	var (
		mu   sync.Mutex
		dsts []string
		wg   sync.WaitGroup
	)
	serve := func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		head := make([]byte, 2)
		if _, err := io.ReadFull(conn, head); err != nil {
			return
		}
		if _, err := io.ReadFull(conn, make([]byte, head[1])); err != nil {
			return
		}
		if _, err := conn.Write([]byte{5, 0}); err != nil {
			return
		}
		req := make([]byte, 4)
		if _, err := io.ReadFull(conn, req); err != nil {
			return
		}
		var dst string
		switch req[3] {
		case 1, 4:
			ip := make([]byte, map[byte]int{1: 4, 4: 16}[req[3]])
			if _, err := io.ReadFull(conn, ip); err != nil {
				return
			}
			a, _ := netip.AddrFromSlice(ip)
			dst = "ip/" + a.String()
		case 3:
			n := make([]byte, 1)
			if _, err := io.ReadFull(conn, n); err != nil {
				return
			}
			name := make([]byte, n[0])
			if _, err := io.ReadFull(conn, name); err != nil {
				return
			}
			dst = "name/" + string(name)
		}
		port := make([]byte, 2)
		if _, err := io.ReadFull(conn, port); err != nil {
			return
		}
		mu.Lock()
		dsts = append(dsts, dst+":"+strconv.Itoa(int(binary.BigEndian.Uint16(port))))
		mu.Unlock()
		_, _ = conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	}
	wg.Go(func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			wg.Go(func() { serve(conn) })
		}
	})
	tb.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	return &url.URL{Scheme: "socks5", Host: ln.Addr().String()}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(dsts)
	}
}

func TestSSRFGuard_SOCKS5(t *testing.T) {
	t.Parallel()

	proxyURL, seen := startSOCKS5(t)
	resolver := &fakeResolver{answers: map[string][][]netip.Addr{
		"example.com":  {addrs("93.184.216.34")},
		"evil.example": {addrs("169.254.169.254")},
	}}
	dial := guardedDialer(t, proxyURL, resolver, defaultGuard(t))

	conn, err := dial(t.Context(), "tcp", "example.com:443")
	require.NoError(t, err)
	_ = conn.Close()

	_, err = dial(t.Context(), "tcp", "evil.example:80")
	require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)

	require.Equal(t, []string{"ip/93.184.216.34:443"}, seen(), "SOCKS5 gets the checked IP, never the name")
}
