// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package client

import (
	"net/http"
	"runtime"
	"time"

	"github.com/altessa-s/go-atlas-net/proxydial"
)

// Transport pool defaults matching go-cleanhttp's DefaultPooledClient.
// Dial-level timeouts (Timeout, KeepAlive) live in
// [net/proxydial] so the pooled transport, the proxy
// dialer, and the gRPC proxy dialer all share one source of truth.
const (
	defaultMaxIdleConns          = 100
	defaultIdleConnTimeout       = 90 * time.Second
	defaultTLSHandshakeTimeout   = 10 * time.Second
	defaultExpectContinueTimeout = 1 * time.Second
)

// DefaultPooledTransport returns a new [http.Transport] configured as the
// client's default one, identically to go-cleanhttp's DefaultPooledTransport:
// connection pooling and keep-alive with the [proxydial.DefaultDialer], TLS
// handshake and Expect: 100-continue timeouts, HTTP/2, environment proxies,
// and per-host idle connections scaled to GOMAXPROCS. Every call returns a
// fresh transport, so a caller can change its dialer, TLS configuration or
// proxy and pass it to [WithTransport] (or an [http.Client] for [WithClient])
// without affecting other clients or losing the pool tuning.
//
// Example:
//
//	tr := httpclient.DefaultPooledTransport()
//	tr.TLSClientConfig = tlsConfig
//	c := httpclient.New(httpclient.WithTransport(tr))
func DefaultPooledTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           proxydial.DefaultDialer().DialContext,
		MaxIdleConns:          defaultMaxIdleConns,
		IdleConnTimeout:       defaultIdleConnTimeout,
		TLSHandshakeTimeout:   defaultTLSHandshakeTimeout,
		ExpectContinueTimeout: defaultExpectContinueTimeout,
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   runtime.GOMAXPROCS(0) + 1,
	}
}
