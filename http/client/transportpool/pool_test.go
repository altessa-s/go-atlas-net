// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package transportpool_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

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
