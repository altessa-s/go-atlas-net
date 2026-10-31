// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client_test

import (
	"crypto/tls"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

func TestDefaultPooledTransport(t *testing.T) {
	t.Parallel()

	tr := httpclient.DefaultPooledTransport()
	require.NotNil(t, tr.DialContext)
	require.NotNil(t, tr.Proxy)
	require.True(t, tr.ForceAttemptHTTP2)
	require.Equal(t, runtime.GOMAXPROCS(0)+1, tr.MaxIdleConnsPerHost)
	require.Positive(t, tr.MaxIdleConns)
	require.Positive(t, tr.IdleConnTimeout)
	require.Positive(t, tr.TLSHandshakeTimeout)
	require.Positive(t, tr.ExpectContinueTimeout)

	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
	tr.MaxIdleConnsPerHost = 1
	fresh := httpclient.DefaultPooledTransport()
	require.NotSame(t, tr, fresh)
	require.Nil(t, fresh.TLSClientConfig, "a change to one transport does not reach the next")
	require.Equal(t, runtime.GOMAXPROCS(0)+1, fresh.MaxIdleConnsPerHost)
}

// A customized default transport works with the default SSRF protection.
func TestDefaultPooledTransportWithClient(t *testing.T) {
	t.Parallel()

	tr := httpclient.DefaultPooledTransport()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	require.NotPanics(t, func() { httpclient.New(httpclient.WithTransport(tr)) })
}
