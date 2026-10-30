// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package client

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"
	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/httpx"
)

// TestClient_FailingHostDoesNotTripOtherHosts pins the per-host isolation
// contract: an unconfigured host whose breaker opens must not reject
// requests to another unconfigured host on the same client.
func TestClient_FailingHostDoesNotTripOtherHosts(t *testing.T) {
	t.Parallel()

	transport := httpx.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "down.test" {
			return nil, errors.New("connection refused")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    req,
		}, nil
	})
	c := New(
		WithoutSSRFProtection(), WithClient(&http.Client{Transport: transport}),
		WithRetryMax(0),
		WithBreakerTimeout(time.Hour),
	)

	do := func(host string) error {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+host+"/", nil)
		require.NoError(t, err)
		resp, err := c.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		return err
	}

	for range DefaultBreakerMinRequests {
		require.Error(t, do("down.test"))
	}
	err := do("down.test")
	require.True(t, IsCircuitBreakerOpen(err), "failing host breaker must be open: %v", err)

	require.NoError(t, do("up.test"), "healthy host must not be rejected by the failing host's breaker")
}

func TestGetBreakerForHost(t *testing.T) {
	t.Parallel()

	newCB := func(opt ...Option) *circuitBreakerClient {
		return newCircuitBreakerClient(*newOptions(opt...))
	}

	t.Run("lazy breaker per host", func(t *testing.T) {
		t.Parallel()
		cb := newCB()
		a := cb.getBreakerForHost("a.test")
		b := cb.getBreakerForHost("b.test")
		require.NotSame(t, a, b)
		require.NotSame(t, cb.CircuitBreaker, a)
		require.Same(t, a, cb.getBreakerForHost("a.test"), "breaker must be reused for the same host")
		require.False(t, cb.sharedUsed.Load())
	})

	t.Run("cap routes overflow hosts to shared breaker", func(t *testing.T) {
		t.Parallel()
		cb := newCB(WithMaxHostBreakers(1))
		require.NotSame(t, cb.CircuitBreaker, cb.getBreakerForHost("a.test"))
		require.Same(t, cb.CircuitBreaker, cb.getBreakerForHost("b.test"))
		require.True(t, cb.sharedUsed.Load())
	})

	t.Run("zero cap shares one breaker", func(t *testing.T) {
		t.Parallel()
		cb := newCB(WithMaxHostBreakers(0))
		require.Same(t, cb.CircuitBreaker, cb.getBreakerForHost("a.test"))
	})

	t.Run("explicit hosts do not consume the cap", func(t *testing.T) {
		t.Parallel()
		cb := newCB(
			WithMaxHostBreakers(1),
			WithCircuitBreakerSettings("explicit.test", &CircuitBreakerSettings{Name: "explicit"}),
		)
		explicit := cb.getBreakerForHost("explicit.test")
		require.Equal(t, "explicit", explicit.Name())
		require.NotSame(t, cb.CircuitBreaker, cb.getBreakerForHost("a.test"))
	})

	t.Run("lazy breaker inherits client settings", func(t *testing.T) {
		t.Parallel()
		cb := newCB(WithBreakerName("api"))
		require.Equal(t, "api-a.test", cb.getBreakerForHost("a.test").Name())
	})
}

func TestBreakersStatesIgnoresIdleSharedBreaker(t *testing.T) {
	t.Parallel()

	cb := newCircuitBreakerClient(*newOptions())
	require.Equal(t, []gobreaker.State{gobreaker.StateClosed}, cb.States(),
		"with no per-host breaker the client-wide one is reported")

	hostCB := cb.getBreakerForHost("a.test")
	for range DefaultBreakerMinRequests {
		_, err := hostCB.Execute(func() (*http.Response, error) {
			return nil, errors.New("forced")
		})
		require.Error(t, err)
	}
	require.Equal(t, gobreaker.StateOpen, hostCB.State())

	// The only used breaker is open; the idle shared one must not be reported.
	require.Equal(t, []gobreaker.State{gobreaker.StateOpen}, cb.States())
	require.Equal(t, gobreaker.StateOpen, cb.HostState("a.test"))
	require.Equal(t, gobreaker.StateClosed, cb.HostState("unknown.test"))
}

func TestBreakersConfiguredHosts(t *testing.T) {
	t.Parallel()

	cb := newCircuitBreakerClient(*newOptions(
		WithCircuitBreakerSettings("b.test", &CircuitBreakerSettings{}),
		WithCircuitBreakerSettings("a.test", &CircuitBreakerSettings{}),
	))
	require.Equal(t, []string{"a.test", "b.test"}, cb.ConfiguredHosts())
	require.Empty(t, newCircuitBreakerClient(*newOptions()).ConfiguredHosts())
}

// TestBreakersStatesCallbackCreatesBreaker guards against a deadlock: State()
// may fire OnStateChange synchronously, and a callback that reaches a new host
// needs the host-map write lock, so States must not hold the read lock then.
func TestBreakersStatesCallbackCreatesBreaker(t *testing.T) {
	t.Parallel()

	var cb *circuitBreakerClient
	settings := &CircuitBreakerSettings{
		Timeout:     10 * time.Millisecond,
		ReadyToTrip: func(gobreaker.Counts) bool { return true },
		OnStateChange: func(_ string, _, to gobreaker.State) {
			if to == gobreaker.StateHalfOpen {
				cb.getBreakerForHost("new.test")
			}
		},
	}
	cb = newCircuitBreakerClient(*newOptions(WithCircuitBreakerSettings("explicit.test", settings)))

	hostCB := cb.getBreakerForHost("explicit.test")
	_, err := hostCB.Execute(func() (*http.Response, error) {
		return nil, errors.New("forced")
	})
	require.Error(t, err)
	time.Sleep(20 * time.Millisecond) // let the open state expire

	done := make(chan []gobreaker.State, 1)
	go func() { done <- cb.States() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("States deadlocked on a state-change callback creating a breaker")
	}
}
