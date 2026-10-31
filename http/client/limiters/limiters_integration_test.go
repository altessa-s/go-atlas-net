//go:build integration

// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package limiters_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/http/client/limiters"
	"github.com/altessa-s/go-atlas-net/http/client/limiters/hostpacer"
)

var errBudget = errors.New("budget spent")

// budget refuses every request of a key after the first n.
func budget(n int) limiters.RequestsLimiter {
	var (
		mu   sync.Mutex
		used = map[string]int{}
	)
	return limiters.Func(func(_ context.Context, key string) error {
		mu.Lock()
		defer mu.Unlock()
		if used[key] >= n {
			return errBudget
		}
		used[key]++
		return nil
	})
}

// TestIntegration_PacerAndBudgetOverRealTransport sends concurrent requests to
// two host names of one loopback server through a chained pacer and budget:
// each host is paced on its own, and requests over budget never reach the
// server.
func TestIntegration_PacerAndBudgetOverRealTransport(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		byHost = map[string]int{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		byHost[r.Host]++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	port := strconv.Itoa(srv.Listener.Addr().(*net.TCPAddr).Port)

	const (
		interval = 20 * time.Millisecond
		allowed  = 4
		perHost  = 6
	)
	pacer, err := hostpacer.New(interval, 1)
	require.NoError(t, err)
	client := &http.Client{Transport: limiters.NewRoundTripper(http.DefaultTransport, limiters.Chain(budget(allowed), pacer))}

	var refused atomic.Int32
	start := time.Now()
	var wg sync.WaitGroup
	for _, host := range []string{"127.0.0.1", "localhost"} {
		for range perHost {
			wg.Go(func() {
				req, rerr := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+host+":"+port+"/", nil)
				if rerr != nil {
					return
				}
				resp, derr := client.Do(req)
				if derr != nil {
					if errors.Is(derr, limiters.ErrRateLimited) && errors.Is(derr, errBudget) {
						refused.Add(1)
					}
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			})
		}
	}
	wg.Wait()
	elapsed := time.Since(start)

	require.Equal(t, int32(2*(perHost-allowed)), refused.Load())
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, map[string]int{"127.0.0.1:" + port: allowed, "localhost:" + port: allowed}, byHost)
	require.GreaterOrEqual(t, elapsed, (allowed-1)*interval, "each host waits for its own slots")
	require.Equal(t, 2, pacer.Len(), "one bucket per host name")
}
