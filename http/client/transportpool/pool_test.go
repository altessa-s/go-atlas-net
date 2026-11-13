// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package transportpool_test

import (
	"context"
	"io"
	"net"
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

// closeNotifyConn reports its Close on a channel.
type closeNotifyConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *closeNotifyConn) Close() error {
	c.once.Do(func() { close(c.closed) })

	return c.Conn.Close()
}

// An HTTP/2 transport cleans a canceled stream up asynchronously, after
// Body.Close has returned, so the cleanups at eviction and at Close can both
// find the connection busy. The delayed cleanup of a retired transport closes
// it even with idle timeouts disabled.
func TestEvictedHTTP2ConnectionClosesAfterCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() //nolint:errcheck // The test server's writer flushes.

		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()

	defer srv.Close()
	defer close(release)

	closed := make(chan struct{})
	h2 := &http.Transport{
		TLSClientConfig:   srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone(), //nolint:errcheck // httptest returns *http.Transport.
		ForceAttemptHTTP2: true,
		IdleConnTimeout:   0,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}

			return &closeNotifyConn{Conn: conn, closed: closed}, nil
		},
	}
	other := &transport{}
	p := transportpool.New(func(ctx context.Context) (string, func() http.RoundTripper) {
		key := ctx.Value(keyType{}).(string) //nolint:errcheck // The tests always set the key.

		return key, func() http.RoundTripper {
			if key == "a" {
				return h2
			}

			return other
		}
	}, transportpool.WithMaxEntries(1), transportpool.WithRetireDelay(50*time.Millisecond))

	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), keyType{}, "a"))
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	held, err := p.RoundTrip(req)
	require.NoError(t, err)
	require.Equal(t, 2, held.ProtoMajor)

	resp, err := p.RoundTrip(request(t, "b")) // evicts the HTTP/2 transport mid-response
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	cancel()

	_ = held.Body.Close()

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the connection of the evicted transport stayed open")
	}
}

// retiring records Retire and counts cleanups.
type retiring struct {
	transport
	retires atomic.Int32
}

func (tr *retiring) Retire() { tr.retires.Add(1) }

// A retired transport is not forgotten after its delayed cleanup: a
// connection that becomes idle later is still reached by the next eviction
// and by CloseIdleConnections, and the oldest retired transport gets one more
// cleanup when it is dropped.
func TestRetiredTransportStaysOwned(t *testing.T) {
	all := map[string]*retiring{"a": {}, "b": {}, "c": {}, "d": {}}
	p := transportpool.New(func(ctx context.Context) (string, func() http.RoundTripper) {
		key := ctx.Value(keyType{}).(string) //nolint:errcheck // The tests always set the key.

		return key, func() http.RoundTripper { return all[key] }
	}, transportpool.WithMaxEntries(1), transportpool.WithRetireDelay(10*time.Millisecond))

	send := func(key string) {
		resp, err := p.RoundTrip(request(t, key))
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}

	send("a")
	send("b") // retires a
	require.EqualValues(t, 1, all["a"].retires.Load())
	require.Eventually(t, func() bool { return all["a"].closes.Load() >= 2 }, time.Second, time.Millisecond,
		"the delayed cleanup of a")

	// Long after the delayed cleanup, a is still cleaned up.
	settled := all["a"].closes.Load()

	p.CloseIdleConnections()
	require.Greater(t, all["a"].closes.Load(), settled)

	settled = all["a"].closes.Load()

	send("c") // retires b: a is dropped (limit 1) with one more cleanup
	require.Greater(t, all["a"].closes.Load(), settled)
	require.EqualValues(t, 1, all["b"].retires.Load())

	settled = all["a"].closes.Load()
	before := all["b"].closes.Load()

	send("d") // retires c: b is dropped with one more cleanup, a is gone
	require.Greater(t, all["b"].closes.Load(), before)
	require.Equal(t, settled, all["a"].closes.Load())
	require.Zero(t, all["d"].retires.Load())
}

// Retire waits for the requests the pool acquired the transport for: a
// transport evicted mid-request is told only after that request finished.
func TestRetireWaitsForAcquiredRequests(t *testing.T) {
	// Promoted fields are valid keys of a composite literal since Go 1.27, and
	// `make vet` (go fix) rewrites the explicit embedded form into this one.
	a := &retiring{entered: make(chan struct{}), resume: make(chan struct{})}
	b := &retiring{}
	p := transportpool.New(func(ctx context.Context) (string, func() http.RoundTripper) {
		key := ctx.Value(keyType{}).(string) //nolint:errcheck // The tests always set the key.

		return key, func() http.RoundTripper {
			if key == "a" {
				return a
			}

			return b
		}
	}, transportpool.WithMaxEntries(1), transportpool.WithRetireDelay(10*time.Millisecond))

	type result struct {
		resp *http.Response
		err  error
	}

	done := make(chan result, 1)

	go func() {
		resp, err := p.RoundTrip(request(t, "a"))
		done <- result{resp, err}
	}()

	<-a.entered

	resp, err := p.RoundTrip(request(t, "b")) // evicts a mid-request
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Zero(t, a.retires.Load(), "retired while a request was in flight")

	close(a.resume)

	held := <-done
	require.NoError(t, held.err)
	require.Zero(t, a.retires.Load(), "retired while a response was open")

	require.NoError(t, held.resp.Body.Close())
	require.EqualValues(t, 1, a.retires.Load())
}

// Terminal reads repeated after the end of a body do not repeat the cleanup,
// and the delayed cleanup of an entry is a single timer: once the pool drops
// a retired transport nothing cleans it up later.
func TestRetiredCleanupIsCoalesced(t *testing.T) {
	all := map[string]*retiring{"a": {}, "b": {}, "c": {}}
	p := transportpool.New(func(ctx context.Context) (string, func() http.RoundTripper) {
		key := ctx.Value(keyType{}).(string) //nolint:errcheck // The tests always set the key.

		return key, func() http.RoundTripper { return all[key] }
	}, transportpool.WithMaxEntries(1), transportpool.WithRetireDelay(50*time.Millisecond))

	held, err := p.RoundTrip(request(t, "a"))
	require.NoError(t, err)

	resp, err := p.RoundTrip(request(t, "b")) // retires a with its body open
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	_, err = io.ReadAll(held.Body)
	require.NoError(t, err)

	atEOF := all["a"].closes.Load()

	for range 5 {
		_, err = held.Body.Read(make([]byte, 1))
		require.ErrorIs(t, err, io.EOF)
	}

	require.Equal(t, atEOF, all["a"].closes.Load(), "repeated terminal reads repeated the cleanup")
	require.NoError(t, held.Body.Close())

	resp, err = p.RoundTrip(request(t, "c")) // retires b: a is dropped, its timer stopped
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	dropped := all["a"].closes.Load()

	time.Sleep(200 * time.Millisecond) // four retire delays: a stopped timer never fires
	require.Equal(t, dropped, all["a"].closes.Load(), "a dropped transport was cleaned up by a leftover timer")
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
