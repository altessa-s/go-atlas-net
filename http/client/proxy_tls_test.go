// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client_test

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

// httpsProxy is an https:// CONNECT proxy that tunnels to the requested
// address and records every CONNECT target.
type httpsProxy struct {
	url   *url.URL
	roots *x509.CertPool

	mu      sync.Mutex
	targets []string
}

func (p *httpsProxy) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.targets)
}

func startHTTPSProxy(tb testing.TB) *httpsProxy {
	tb.Helper()

	p := &httpsProxy{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		p.mu.Lock()
		p.targets = append(p.targets, r.Host)
		p.mu.Unlock()
		upstream, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = upstream.Close() }()
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		deadline := time.Now().Add(5 * time.Second)
		_ = conn.SetDeadline(deadline)
		_ = upstream.SetDeadline(deadline)
		if _, err = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			return
		}
		go func() { _, _ = io.Copy(upstream, rw) }()
		_, _ = io.Copy(conn, upstream)
	}))
	tb.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(tb, err)
	p.url = u
	p.roots = x509.NewCertPool()
	p.roots.AddCert(srv.Certificate())
	return p
}

// TestWithProxyTLSConfig_TunnelsThroughHTTPSProxy covers the custom dialer
// that a proxy TLS configuration installs: the proxy is resolved per
// connection and reached over TLS verified with the given roots.
func TestWithProxyTLSConfig_TunnelsThroughHTTPSProxy(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "backend")
	}))
	t.Cleanup(backend.Close)
	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)

	errResolve := errors.New("no proxy route")
	cases := []struct {
		name        string
		proxyFn     func(p *httpsProxy) httpclient.ProxyFunc
		wantErr     error
		wantAnyErr  bool
		wantTargets func(p *httpsProxy) []string
	}{
		{
			name:        "https_proxy",
			proxyFn:     func(p *httpsProxy) httpclient.ProxyFunc { return http.ProxyURL(p.url) },
			wantTargets: func(*httpsProxy) []string { return []string{backendURL.Host} },
		},
		{
			name: "direct_when_resolver_opts_out",
			proxyFn: func(*httpsProxy) httpclient.ProxyFunc {
				return func(*http.Request) (*url.URL, error) { return nil, nil } //nolint:nilnil // http.Transport.Proxy contract
			},
			wantTargets: func(*httpsProxy) []string { return nil },
		},
		{
			name: "resolver_error",
			proxyFn: func(*httpsProxy) httpclient.ProxyFunc {
				return func(*http.Request) (*url.URL, error) { return nil, errResolve }
			},
			wantErr:     errResolve,
			wantTargets: func(*httpsProxy) []string { return nil },
		},
		{
			name: "unsupported_scheme",
			proxyFn: func(*httpsProxy) httpclient.ProxyFunc {
				return http.ProxyURL(&url.URL{Scheme: "ftp", Host: "proxy.example:21"})
			},
			wantAnyErr:  true,
			wantTargets: func(*httpsProxy) []string { return nil },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proxy := startHTTPSProxy(t)
			c := httpclient.NewHTTPClient(
				httpclient.WithoutSSRFProtection(),
				httpclient.WithRetryMax(0),
				httpclient.WithProxyFunc(tc.proxyFn(proxy)),
				httpclient.WithProxyTLSConfig(&tls.Config{RootCAs: proxy.roots}),
			)

			resp, err := c.Get(t.Context(), backend.URL)
			switch {
			case tc.wantErr != nil:
				require.ErrorIs(t, err, tc.wantErr)
			case tc.wantAnyErr:
				require.Error(t, err)
			default:
				require.NoError(t, err)
				body, rerr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				require.NoError(t, rerr)
				require.Equal(t, "backend", string(body))
			}
			require.Equal(t, tc.wantTargets(proxy), proxy.seen())
		})
	}
}
