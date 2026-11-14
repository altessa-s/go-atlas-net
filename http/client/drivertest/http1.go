// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package drivertest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/http/client"
)

func testH1WarmedReplay(t *testing.T, f func(*testing.T) client.TransportDriver) {
	for _, withoutRetry := range []bool{false, true} {
		for _, tc := range []h1ReplayCase{
			{name: "POST", method: http.MethodPost, max: 1},
			{name: "PATCH", method: http.MethodPatch, max: 1},
			{name: "GET", method: http.MethodGet, max: twoConnections},
			{name: "key/rewindable", method: http.MethodPost, key: "Idempotency-Key", max: twoConnections},
			{name: "x-key/rewindable", method: http.MethodPost, key: "X-Idempotency-Key", max: twoConnections},
			{name: "key/stream", method: http.MethodPost, key: "Idempotency-Key", stream: true, max: 1},
		} {
			name := "warmed-h1/" + map[bool]string{false: "plain/", true: "WithoutRetry/"}[withoutRetry] + tc.name
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				testH1WarmedReplayCase(t, f, withoutRetry, tc)
			})
		}
	}
}

type h1ReplayCase struct {
	name, method, key string
	stream            bool
	max               int32
}

func testH1WarmedReplayCase(t *testing.T, f func(*testing.T) client.TransportDriver, withoutRetry bool, tc h1ReplayCase) {
	var hits, completed atomic.Int32

	// The warm-up address is published before its response completes.
	// Only the first failing request reads it; retries just count.
	var warmAddress atomic.Value

	s := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/warm" {
			warmAddress.Store(r.RemoteAddr)

			_, _ = io.WriteString(w, "ok")

			return
		}

		n := hits.Add(1)
		if n == 1 && r.RemoteAddr != warmAddress.Load() {
			t.Error("first failing request did not reuse the warmed connection")
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading complete request: %v", err)
		} else {
			completed.Add(1)

			want := payload
			if tc.method == http.MethodGet {
				want = ""
			}

			if string(body) != want {
				t.Errorf("request body = %q, want %q", body, want)
			}
		}

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("HTTP/1.1 response does not support hijacking")
			return
		}

		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijacking HTTP/1.1 connection: %v", err)
			return
		}

		_ = conn.Close()
	}), nil, false)
	env := environment(s)
	env.TLS.NextProtos = []string{protocolHTTP1}

	var c *http.Client
	if withoutRetry {
		c = client.New(client.WithoutSSRFProtection(), client.WithoutProxy(), client.WithoutRetry(),
			client.WithTransportDriver(f(t)),
			client.WithTransport(&http.Transport{DialContext: env.Dial, TLSClientConfig: env.TLS}))
		t.Cleanup(c.CloseIdleConnections)
	} else {
		c = &http.Client{Transport: transport(t, f(t), env, false)}
	}

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	warm, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+authority+"/warm", nil)
	require.NoError(t, err)
	resp, err := c.Do(warm)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	require.NoError(t, err)
	require.Equal(t, 1, resp.ProtoMajor)

	var body io.Reader
	if tc.method != http.MethodGet {
		body = strings.NewReader(payload)
		if tc.stream {
			body = io.LimitReader(body, int64(len(payload)))
		}
	}

	req, err := http.NewRequestWithContext(ctx, tc.method, "https://"+authority+"/fail", body)
	require.NoError(t, err)

	if body != nil {
		require.Equal(t, !tc.stream, req.GetBody != nil)
	}

	if tc.key != "" {
		req.Header.Set(tc.key, "replay-conformance")
	}

	resp, err = c.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}

	require.Error(t, err, "a complete request dropped without a response must fail")

	count := hits.Load()
	require.GreaterOrEqual(t, count, int32(1))
	require.LessOrEqual(t, count, tc.max)
	require.Equal(t, count, completed.Load(), "all transmissions must be read completely")

	// Keep the context and server alive to expose retries after return.
	time.Sleep(replaySettling)
	require.Equal(t, count, hits.Load(), "transmissions grew after the request returned")
}

const h1WarmConnections = 3
const h1IdempotencyKey = "Idempotency-Key"

func testH1SeveralWarmedReplay(t *testing.T, f func(*testing.T) client.TransportDriver) {
	for _, withoutRetry := range []bool{false, true} {
		for _, tc := range []h1ReplayCase{
			{name: "POST", method: http.MethodPost},
			{name: "GET", method: http.MethodGet},
			{name: "key/rewindable", method: http.MethodPost, key: h1IdempotencyKey},
			{name: "x-key/rewindable", method: http.MethodPost, key: "X-Idempotency-Key"},
		} {
			name := "several-warmed-h1/" + map[bool]string{false: "plain/", true: "WithoutRetry/"}[withoutRetry] + tc.name
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				testH1SeveralWarmedReplayCase(t, f, withoutRetry, tc)
			})
		}
	}
}

func testH1SeveralWarmedReplayCase(t *testing.T, f func(*testing.T) client.TransportDriver, withoutRetry bool, tc h1ReplayCase) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	var mu sync.Mutex

	warmed := make(map[string]bool)
	transmissions := make(map[string]int)
	allArrived := make(chan struct{})

	var hits, completed atomic.Int32

	s := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/warm" {
			mu.Lock()
			warmed[r.RemoteAddr] = true

			if len(warmed) == h1WarmConnections {
				close(allArrived)
			}
			mu.Unlock()

			// No connection can be reused for another warm-up request until
			// all concurrent requests have arrived on separate connections.
			select {
			case <-allArrived:
				_, _ = io.WriteString(w, "ok")
			case <-ctx.Done():
			}

			return
		}

		n := hits.Add(1)

		mu.Lock()
		transmissions[r.RemoteAddr]++

		if n == 1 && !warmed[r.RemoteAddr] {
			t.Error("first failing request did not reuse a warmed connection")
		}
		mu.Unlock()

		if readH1ReplayBody(t, r, tc.method) {
			completed.Add(1)
		}

		// Drop even fresh connections: the chosen terminal outcome is an
		// error, rather than a successful response on a fresh connection.
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("HTTP/1.1 response does not support hijacking")
			return
		}

		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijacking HTTP/1.1 connection: %v", err)
			return
		}

		_ = conn.Close()
	}), nil, false)
	env := environment(s)
	env.TLS.NextProtos = []string{protocolHTTP1}
	env.MaxConnsPerHost = h1WarmConnections
	env.MaxIdleConnsPerHost = h1WarmConnections
	env.MaxIdleConns = h1WarmConnections
	env.ResponseHeaderTimeout = testTimeout

	var c *http.Client
	if withoutRetry {
		c = client.New(client.WithoutSSRFProtection(), client.WithoutProxy(), client.WithoutRetry(),
			client.WithTransportDriver(f(t)), client.WithTransport(&http.Transport{
				DialContext: env.Dial, TLSClientConfig: env.TLS,
				MaxConnsPerHost: h1WarmConnections, MaxIdleConnsPerHost: h1WarmConnections, MaxIdleConns: h1WarmConnections,
			}))
		t.Cleanup(c.CloseIdleConnections)
	} else {
		c = &http.Client{Transport: transport(t, f(t), env, false)}
	}

	warmH1Connections(t, ctx, c)

	mu.Lock()
	warmCount := len(warmed)
	mu.Unlock()

	require.Equal(t, h1WarmConnections, warmCount)

	var body io.Reader
	if tc.method != http.MethodGet {
		body = strings.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, tc.method, "https://"+authority+"/fail", body)
	require.NoError(t, err)

	if body != nil {
		require.NotNil(t, req.GetBody)
	}

	if tc.key != "" {
		req.Header.Set(tc.key, "replay-conformance")
	}

	resp, err := c.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}

	require.Error(t, err, "all complete requests are dropped without a response")
	require.NoError(t, ctx.Err(), "request must terminate before its deadline")

	count := hits.Load()
	t.Logf("Go HTTP/1.1 replay: %d transmissions after warming %d connections", count, warmCount)

	require.GreaterOrEqual(t, count, int32(1))
	require.LessOrEqual(t, count, int32(h1WarmConnections+1))

	if tc.method == http.MethodPost && tc.key == "" {
		require.Equal(t, int32(1), count)
	}

	require.Equal(t, count, completed.Load(), "all transmissions must be read completely")

	// Leave the context and server alive to catch transmissions after return.
	time.Sleep(replaySettling)
	require.Equal(t, count, hits.Load(), "transmissions grew after the request returned")

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, transmissions, int(count), "every transmission must use a distinct connection")
}

func warmH1Connections(t *testing.T, ctx context.Context, c *http.Client) {
	t.Helper()

	results := make(chan error, h1WarmConnections)
	for range h1WarmConnections {
		go func() {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+authority+"/warm", nil)
			if err != nil {
				results <- err
				return
			}

			resp, err := c.Do(req)
			if err != nil {
				results <- err
				return
			}

			_, err = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()

			if resp.ProtoMajor != 1 {
				err = fmt.Errorf("warm-up protocol = %s, want HTTP/1.1", resp.Proto)
			}

			results <- err
		}()
	}

	// Drain every result before failing, so no warm-up goroutine outlives us.
	for range h1WarmConnections {
		if err := <-results; err != nil {
			t.Errorf("warming HTTP/1.1 connection: %v", err)
		}
	}

	if t.Failed() {
		t.FailNow()
	}
}

func readH1ReplayBody(t *testing.T, r *http.Request, method string) bool {
	t.Helper()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("reading complete request: %v", err)
		return false
	}

	want := payload
	if method == http.MethodGet {
		want = ""
	}

	if string(body) != want {
		t.Errorf("request body = %q, want %q", body, want)
	}

	return true
}
