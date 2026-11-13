// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package drivertest_test

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"slices"
	"testing"

	"github.com/altessa-s/go-atlas-net/http/client"
	"github.com/altessa-s/go-atlas-net/http/client/drivertest"
)

type referenceDriver struct{}

func (referenceDriver) NewTransport(env client.DriverEnv) (http.RoundTripper, error) {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)

	if env.TLS != nil && len(env.TLS.NextProtos) > 0 {
		protocols.SetHTTP1(false)
		protocols.SetHTTP2(false)

		for _, p := range env.TLS.NextProtos {
			switch p {
			case "h2":
				protocols.SetHTTP2(true)
			case "http/1.1":
				protocols.SetHTTP1(true)
			}
		}
	}

	return &http.Transport{DialContext: env.Dial, TLSClientConfig: env.TLS, Protocols: protocols,
		MaxIdleConns: env.MaxIdleConns, MaxIdleConnsPerHost: env.MaxIdleConnsPerHost, MaxConnsPerHost: env.MaxConnsPerHost,
		IdleConnTimeout: env.IdleConnTimeout, TLSHandshakeTimeout: env.TLSHandshakeTimeout, ResponseHeaderTimeout: env.ResponseHeaderTimeout,
		ExpectContinueTimeout: env.ExpectContinueTimeout, DisableKeepAlives: env.DisableKeepAlives, DisableCompression: env.DisableCompression}, nil
}
func TestReferenceDriver(t *testing.T) {
	drivertest.Run(t, func(*testing.T) client.TransportDriver { return referenceDriver{} })
}

// Exercise the public client construction path, including transport preparation,
// before observing both the driver's policy and the actual TLS ClientHello.
func TestClientDestinationALPN(t *testing.T) {
	for _, protos := range [][]string{{"http/1.1"}, {"h2"}, nil} {
		for _, ssrf := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/ssrf=%t", protos, ssrf), func(t *testing.T) {
				hello := make(chan []string, 1)
				s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
				}))
				s.EnableHTTP2 = true
				s.TLS = &tls.Config{GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
					hello <- slices.Clone(info.SupportedProtos)
					return nil, nil
				}}

				s.StartTLS()
				defer s.Close()

				roots := x509.NewCertPool()
				roots.AddCert(s.Certificate())

				base := client.DefaultPooledTransport()
				base.TLSClientConfig = &tls.Config{RootCAs: roots, NextProtos: slices.Clone(protos)}

				opts := []client.Option{client.WithTransport(base), client.WithTransportDriver(inspectingDriver{t, protos}),
					client.WithoutProxy(), client.WithoutRetry(),
					client.WithSSRFAllowedCIDRs(netip.MustParsePrefix("127.0.0.0/8"))}
				if !ssrf {
					opts = append(opts, client.WithoutSSRFProtection())
				}

				c := client.New(opts...)
				defer c.CloseIdleConnections()

				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.URL, nil)
				if err != nil {
					t.Fatal(err)
				}

				res, err := c.Do(req)
				if err != nil {
					t.Fatal(err)
				}

				res.Body.Close()

				got := <-hello
				if protos != nil && !slices.Equal(got, protos) {
					t.Fatalf("ClientHello ALPN = %v; want %v", got, protos)
				}

				if !reflect.DeepEqual(base.TLSClientConfig.NextProtos, protos) {
					t.Fatalf("caller ALPN changed: %v", base.TLSClientConfig.NextProtos)
				}
			})
		}
	}
}

type inspectingDriver struct {
	t      *testing.T
	protos []string
}

func (d inspectingDriver) NewTransport(env client.DriverEnv) (http.RoundTripper, error) {
	if env.TLS == nil || !reflect.DeepEqual(env.TLS.NextProtos, d.protos) {
		d.t.Fatalf("driver TLS = %#v; want ALPN %#v", env.TLS, d.protos)
	}

	return (referenceDriver{}).NewTransport(env)
}
