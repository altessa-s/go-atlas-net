//go:build integration

// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client_test

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"log"
	"math/big"
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

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

// itPKI is a throwaway certificate authority with a loopback server
// certificate and a client certificate, generated per test.
type itPKI struct {
	roots  *x509.CertPool
	server tls.Certificate
	client tls.Certificate
}

func newITPKI(tb testing.TB) *itPKI {
	tb.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(tb, err)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "go-atlas-net integration CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(tb, err)
	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(tb, err)

	issue := func(serial int64, cn string, usage x509.ExtKeyUsage, ips []net.IP, dns []string) tls.Certificate {
		key, kerr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(tb, kerr)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{usage},
			IPAddresses:  ips,
			DNSNames:     dns,
		}
		der, cerr := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		require.NoError(tb, cerr)
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	}

	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	return &itPKI{
		roots:  roots,
		server: issue(2, "localhost", x509.ExtKeyUsageServerAuth, []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}, []string{"localhost"}),
		client: issue(3, "integration-client", x509.ExtKeyUsageClientAuth, nil, nil),
	}
}

// itTLSServer starts an HTTPS server with the PKI's server certificate,
// configured further by tweak, and counts the TLS handshakes it starts.
func itTLSServer(tb testing.TB, pki *itPKI, h http.Handler, tweak func(*tls.Config)) (*httptest.Server, *atomic.Int32) {
	tb.Helper()

	var handshakes atomic.Int32
	srv := httptest.NewUnstartedServer(h)
	cfg := &tls.Config{Certificates: []tls.Certificate{pki.server}, MinVersion: tls.VersionTLS12}
	if tweak != nil {
		tweak(cfg)
	}
	cfg.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		handshakes.Add(1)
		return nil, nil //nolint:nilnil // nil config means "use the server config"
	}
	srv.TLS = cfg
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // rejected handshakes are expected
	srv.StartTLS()
	tb.Cleanup(srv.Close)
	return srv, &handshakes
}

// itTransport returns a pooled transport trusting the PKI's CA, presenting
// certs as client certificates.
func itTransport(pki *itPKI, certs ...tls.Certificate) *http.Transport {
	t := httpclient.DefaultPooledTransport()
	t.TLSClientConfig = &tls.Config{RootCAs: pki.roots, Certificates: certs, MinVersion: tls.VersionTLS12}
	return t
}

var itLoopback = httpclient.WithSSRFAllowedCIDRs(netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128"))

func itGet(tb testing.TB, c httpclient.HTTPClient, rawURL string) (int, string, error) {
	tb.Helper()

	resp, err := c.Get(tb.Context(), rawURL)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

func TestIntegration_TLSCustomRootCA(t *testing.T) {
	t.Parallel()

	pki := newITPKI(t)
	srv, handshakes := itTLSServer(t, pki, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.TLS.ServerName+"|"+tls.VersionName(r.TLS.Version))
	}), nil)

	t.Run("trusted_root", func(t *testing.T) {
		c := httpclient.NewHTTPClient(itLoopback, httpclient.WithTransport(itTransport(pki)), httpclient.WithRetryMax(0))
		status, body, err := itGet(t, c, srv.URL)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "|TLS 1.3", body, "an IP target sends no SNI and negotiates TLS 1.3")

		localhost := "https://localhost:" + strconv.Itoa(srv.Listener.Addr().(*net.TCPAddr).Port)
		status, body, err = itGet(t, c, localhost)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "localhost|TLS 1.3", body)
	})

	t.Run("unknown_authority_is_not_retried", func(t *testing.T) {
		before := handshakes.Load()
		c := httpclient.NewHTTPClient(itLoopback, httpclient.WithRetryMax(3), httpclient.WithRetryWait(time.Millisecond, time.Millisecond))
		_, _, err := itGet(t, c, srv.URL)
		var verr *tls.CertificateVerificationError
		require.ErrorAs(t, err, &verr)
		require.Equal(t, int32(1), handshakes.Load()-before, "a rejected certificate must not be retried")
	})
}

func TestIntegration_MutualTLS(t *testing.T) {
	t.Parallel()

	pki := newITPKI(t)
	var served atomic.Int32
	srv, _ := itTLSServer(t, pki, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		_, _ = io.WriteString(w, r.TLS.PeerCertificates[0].Subject.CommonName)
	}), func(cfg *tls.Config) {
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		cfg.ClientCAs = pki.roots
	})

	t.Run("client_certificate", func(t *testing.T) {
		c := httpclient.NewHTTPClient(itLoopback, httpclient.WithTransport(itTransport(pki, pki.client)), httpclient.WithRetryMax(0))
		status, body, err := itGet(t, c, srv.URL)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "integration-client", body)
	})

	t.Run("missing_client_certificate", func(t *testing.T) {
		before := served.Load()
		c := httpclient.NewHTTPClient(itLoopback, httpclient.WithTransport(itTransport(pki)), httpclient.WithRetryMax(0))
		_, _, err := itGet(t, c, srv.URL)
		require.Error(t, err)
		require.Equal(t, before, served.Load(), "the handler must not run without a client certificate")
	})
}

// itConnectProxy is a plain HTTP CONNECT proxy on loopback. When user is set
// it requires Basic proxy credentials and answers 407 otherwise.
type itConnectProxy struct {
	user, pass string

	url      *url.URL
	mu       sync.Mutex
	connects []string
	open     map[net.Conn]struct{}
	denied   atomic.Int32
}

func (p *itConnectProxy) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.connects)
}

func startITConnectProxy(tb testing.TB, user, pass string) *itConnectProxy {
	tb.Helper()

	p := &itConnectProxy{user: user, pass: pass, open: make(map[net.Conn]struct{})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(tb, err)
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			p.mu.Lock()
			p.open[conn] = struct{}{}
			p.mu.Unlock()
			wg.Go(func() { p.serve(conn) })
		}
	})
	tb.Cleanup(func() {
		_ = ln.Close()
		// Tunnels kept alive by the client's connection pool end here.
		p.mu.Lock()
		for conn := range p.open {
			_ = conn.Close()
		}
		p.mu.Unlock()
		wg.Wait()
	})
	p.url = &url.URL{Scheme: "http", Host: ln.Addr().String()}
	return p
}

func (p *itConnectProxy) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	deadline := time.Now().Add(10 * time.Second)
	_ = conn.SetDeadline(deadline)
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil || req.Method != http.MethodConnect {
		return
	}
	if p.user != "" {
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(p.user+":"+p.pass))
		if req.Header.Get("Proxy-Authorization") != want {
			p.denied.Add(1)
			_, _ = io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"it\"\r\nContent-Length: 0\r\n\r\n")
			return
		}
	}
	p.mu.Lock()
	p.connects = append(p.connects, req.Host)
	p.mu.Unlock()
	upstream, err := net.DialTimeout("tcp", req.Host, 5*time.Second)
	if err != nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer func() { _ = upstream.Close() }()
	_ = upstream.SetDeadline(deadline)
	if _, err = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	go func() {
		_, _ = io.Copy(upstream, br)
		_ = upstream.Close() // the client side is gone: end the other direction too
	}()
	_, _ = io.Copy(conn, upstream)
}

// TestIntegration_ProxyCONNECTToTLSTarget sends HTTPS requests through a
// local CONNECT proxy: the destination TLS runs end to end inside the tunnel,
// proxy credentials come from the proxy URL, and a literal private target is
// refused before the proxy is contacted.
func TestIntegration_ProxyCONNECTToTLSTarget(t *testing.T) {
	t.Parallel()

	pki := newITPKI(t)
	srv, _ := itTLSServer(t, pki, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "secure:"+r.URL.Path)
	}), nil)
	target, err := url.Parse(srv.URL)
	require.NoError(t, err)

	t.Run("authenticated_proxy", func(t *testing.T) {
		t.Parallel()

		proxy := startITConnectProxy(t, "svc", "pa:ss")
		proxyURL := *proxy.url
		proxyURL.User = url.UserPassword("svc", "pa:ss")
		c := httpclient.NewHTTPClient(itLoopback, httpclient.WithRetryMax(0),
			httpclient.WithTransport(itTransport(pki)), httpclient.WithProxyURL(&proxyURL))

		status, body, err := itGet(t, c, srv.URL+"/a")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "secure:/a", body)
		require.Equal(t, []string{target.Host}, proxy.seen())
	})

	t.Run("missing_proxy_credentials", func(t *testing.T) {
		t.Parallel()

		proxy := startITConnectProxy(t, "svc", "pa:ss")
		c := httpclient.NewHTTPClient(itLoopback, httpclient.WithRetryMax(0),
			httpclient.WithTransport(itTransport(pki)), httpclient.WithProxyURL(proxy.url))

		_, _, err := itGet(t, c, srv.URL+"/a")
		require.Error(t, err)
		require.Equal(t, int32(1), proxy.denied.Load())
		require.Empty(t, proxy.seen())
	})

	t.Run("private_literal_target_refused_before_proxy", func(t *testing.T) {
		t.Parallel()

		proxy := startITConnectProxy(t, "", "")
		c := httpclient.NewHTTPClient(httpclient.WithSSRFAllowedCIDRs(netip.MustParsePrefix("127.0.0.1/32")), httpclient.WithRetryMax(0),
			httpclient.WithTransport(itTransport(pki)), httpclient.WithProxyURL(proxy.url))

		_, _, err := itGet(t, c, "https://10.255.255.1/admin")
		require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)
		require.Empty(t, proxy.seen())
	})
}
