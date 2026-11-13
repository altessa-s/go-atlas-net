// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package transportpool_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/http/client/transportpool"
)

type keyType struct{}
type transport struct {
	closes  atomic.Int32
	entered chan struct{}
	resume  chan struct{}
}

func (tr *transport) RoundTrip(*http.Request) (*http.Response, error) {
	if tr.entered != nil {
		close(tr.entered)
		<-tr.resume
	}

	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
}
func (tr *transport) CloseIdleConnections() { tr.closes.Add(1) }
func request(t *testing.T, key string) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(context.WithValue(t.Context(), keyType{}, key), "GET", "https://example.test", nil)
	require.NoError(t, err)

	return r
}
func TestPoolConcurrentBuildAndLRU(t *testing.T) {
	var builds atomic.Int32

	a, b, c := &transport{}, &transport{}, &transport{}
	all := map[string]*transport{"a": a, "b": b, "c": c}
	p := transportpool.New(func(ctx context.Context) (string, func() http.RoundTripper) {
		key := ctx.Value(keyType{}).(string)
		return key, func() http.RoundTripper { builds.Add(1); return all[key] }
	}, transportpool.WithMaxEntries(2))

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			resp, err := p.RoundTrip(request(t, "a"))
			if err == nil {
				_ = resp.Body.Close()
			}

			require.NoError(t, err)
		})
	}

	wg.Wait()
	require.EqualValues(t, 1, builds.Load())

	for _, k := range []string{"b", "a", "c"} {
		resp, err := p.RoundTrip(request(t, k))
		require.NoError(t, err)

		_ = resp.Body.Close()
	}

	require.EqualValues(t, 1, b.closes.Load())
	require.Zero(t, a.closes.Load())
	p.CloseIdleConnections()
	require.EqualValues(t, 1, a.closes.Load())
	require.EqualValues(t, 1, c.closes.Load())
}
func TestEvictedInflightFinishesAndCloses(t *testing.T) {
	a := &transport{entered: make(chan struct{}), resume: make(chan struct{})}
	b := &transport{}
	p := transportpool.New(func(ctx context.Context) (string, func() http.RoundTripper) {
		key := ctx.Value(keyType{}).(string)

		return key, func() http.RoundTripper {
			if key == "a" {
				return a
			}

			return b
		}
	}, transportpool.WithMaxEntries(1))

	done := make(chan struct{})
	go func() {
		defer close(done)

		resp, err := p.RoundTrip(request(t, "a"))
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	<-a.entered

	resp, err := p.RoundTrip(request(t, "b"))
	require.NoError(t, err)

	_ = resp.Body.Close()

	require.EqualValues(t, 1, a.closes.Load())
	close(a.resume)
	<-done
	require.EqualValues(t, 2, a.closes.Load())
}

// A transport can keep its connection busy at the end of the body and release
// it only in Close, so an evicted transport is cleaned up again after Close.
func TestEvictedCleanupRepeatsAfterClose(t *testing.T) {
	a, b := &transport{}, &transport{}
	p := transportpool.New(func(ctx context.Context) (string, func() http.RoundTripper) {
		key := ctx.Value(keyType{}).(string)

		return key, func() http.RoundTripper {
			if key == "a" {
				return a
			}

			return b
		}
	}, transportpool.WithMaxEntries(1))

	held, err := p.RoundTrip(request(t, "a"))
	require.NoError(t, err)

	resp, err := p.RoundTrip(request(t, "b")) // evicts a while its body is open
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.EqualValues(t, 1, a.closes.Load())

	_, err = io.ReadAll(held.Body)
	require.NoError(t, err)

	atEOF := a.closes.Load()
	require.Greater(t, atEOF, int32(1))

	require.NoError(t, held.Body.Close())
	require.Greater(t, a.closes.Load(), atEOF)
}

func TestPoolUpgrade(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()

		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		_ = rw.Flush()

		buf := make([]byte, 4)
		if _, err = io.ReadFull(rw, buf); err == nil {
			_, _ = conn.Write(buf)
		}
	}))
	defer s.Close()

	tr := &http.Transport{}

	p := transportpool.New(func(context.Context) (string, func() http.RoundTripper) {
		return "upgrade", func() http.RoundTripper { return tr }
	})
	defer p.CloseIdleConnections()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "echo")
	resp, err := p.RoundTrip(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	body, ok := resp.Body.(io.ReadWriteCloser)
	require.True(t, ok)

	_, err = body.Write([]byte("ping"))
	require.NoError(t, err)

	buf := make([]byte, 4)
	_, err = io.ReadFull(body, buf)
	require.NoError(t, err)
	require.Equal(t, "ping", string(buf))
}
