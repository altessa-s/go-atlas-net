// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package drivertest_test

import (
	"net/http"
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
