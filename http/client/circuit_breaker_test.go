// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
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

func TestBreakerIsSuccessful(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"shared", "lazy", "configured"} {
		for _, tc := range []struct {
			name        string
			custom      bool
			explicitNil bool
			marked      bool
			success     bool
		}{
			{name: "default marked error", marked: true},
			{name: "nil marked error", explicitNil: true, marked: true},
			{name: "nil transport failure", explicitNil: true},
			{name: "nil success", explicitNil: true, success: true},
			{name: "marked error is successful", custom: true, marked: true},
			{name: "transport failure is unsuccessful", custom: true},
			{name: "nil error is successful", custom: true, success: true},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				t.Parallel()

				cause := errors.New("private-transport-detail")

				var transportErr error
				if !tc.success {
					transportErr = cause
					if tc.marked {
						transportErr = fmt.Errorf("transport: %w", &NonRetryableError{Err: cause})
					}
				}

				var attempts, classified int

				transport := httpx.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
					attempts++

					if transportErr != nil {
						return nil, transportErr
					}

					return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
				})
				opts := []Option{
					WithoutSSRFProtection(), WithClient(&http.Client{Transport: transport}),
					WithBreakerInterval(time.Hour), WithBreakerTimeout(time.Hour),
				}

				switch mode {
				case "shared":
					opts = append(opts, WithMaxHostBreakers(0))
				case "configured":
					opts = append(opts, WithCircuitBreakerSettings("api.test:8080", &CircuitBreakerSettings{
						Interval: time.Hour, Timeout: time.Hour,
					}))
				}

				if tc.custom {
					opts = append(opts, WithBreakerIsSuccessful(func(err error) bool {
						classified++

						require.True(t, err == transportErr, "predicate must receive the original transport error")

						if err == nil {
							return true
						}

						require.ErrorIs(t, err, cause)

						var marked *NonRetryableError

						return errors.As(err, &marked)
					}))
				} else if tc.explicitNil {
					opts = append(opts, WithBreakerIsSuccessful(nil))
				}

				cb := newCircuitBreakerClient(*newOptions(opts...))
				c := cb.standardClient()
				do := func() error {
					req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://api.test:8080/", nil)
					require.NoError(t, err)

					resp, err := c.Do(req)
					if resp != nil {
						_ = resp.Body.Close()
					}

					return err
				}

				for range DefaultBreakerMinRequests {
					err := do()
					if tc.success {
						require.NoError(t, err)
					} else {
						require.ErrorIs(t, err, cause, "a result counted as successful must still return its error")
						require.NotContains(t, err.Error(), cause.Error())

						if tc.marked {
							require.ErrorIs(t, err, ErrNonRetryable)
						}
					}
				}

				breaker := cb.getBreakerForHost("api.test:8080")
				if mode == "shared" {
					require.Same(t, cb.CircuitBreaker, breaker)
				} else {
					require.NotSame(t, cb.CircuitBreaker, breaker)
				}

				if tc.custom {
					require.Equal(t, DefaultBreakerMinRequests, classified)
				}

				if tc.success || (tc.custom && tc.marked) {
					require.Equal(t, gobreaker.StateClosed, breaker.State())
					require.Equal(t, uint32(DefaultBreakerMinRequests), breaker.Counts().TotalSuccesses)

					transportErr = nil

					require.NoError(t, do(), "healthy requests must still reach the transport")
					require.Equal(t, DefaultBreakerMinRequests+1, attempts)
				} else {
					require.Equal(t, gobreaker.StateOpen, breaker.State())
					require.ErrorIs(t, do(), gobreaker.ErrOpenState)
					require.Equal(t, DefaultBreakerMinRequests, attempts, "open breaker must reject before transport")
				}
			})
		}
	}
}

func TestBreakerIsSuccessfulPanic(t *testing.T) {
	t.Parallel()

	cause := errors.New("transport failure")

	var calls int

	cb := newCircuitBreakerClient(*newOptions(WithBreakerIsSuccessful(func(err error) bool {
		calls++

		if err == cause {
			panic("predicate panic")
		}

		// gobreaker's recovery invokes the predicate with the panic as an error.
		require.EqualError(t, err, "predicate panic")

		return false
	})))
	rt := cb.newCircuitBreakerRoundTripper(httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, cause
	}))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://api.test/", nil)
	require.NoError(t, err)
	require.PanicsWithValue(t, "predicate panic", func() {
		_, _ = rt.RoundTrip(req)
	})
	require.Equal(t, 2, calls)
}

type breakerSelfCause struct{}

func (e *breakerSelfCause) Error() string { return "self" }
func (e *breakerSelfCause) Unwrap() error { return e }

func TestBreakerIsSuccessfulUninspectableCauses(t *testing.T) {
	t.Parallel()

	doubled := errors.New("transport failure")
	for range 40 {
		doubled = errors.Join(doubled, doubled)
	}

	for name, cause := range map[string]error{"cyclic": &breakerSelfCause{}, "shared": doubled} {
		for _, custom := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/custom=%t", name, custom), func(t *testing.T) {
				t.Parallel()

				classified := make(chan error, 1)
				started := make(chan struct{})
				opts := []Option{WithBreakerInterval(time.Hour)}

				if custom {
					opts = append(opts, WithBreakerIsSuccessful(func(err error) bool {
						if err != nil {
							classified <- err
						}

						var refusal *NonRetryableError

						return err == nil || errors.As(err, &refusal)
					}))
				} else {
					opts = append(opts, WithBreakerIsSuccessful(nil))
				}

				cb := newCircuitBreakerClient(*newOptions(opts...))
				rt := cb.newCircuitBreakerRoundTripper(httpx.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Path == "/bad" {
						close(started)
						return nil, cause
					}

					return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
				}))

				badReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://api.test/bad", nil)
				if err != nil {
					t.Fatal(err)
				}

				goodReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://api.test/good", nil)
				if err != nil {
					t.Fatal(err)
				}

				do := func(req *http.Request, done chan<- error) {
					resp, err := rt.RoundTrip(req)
					if resp != nil {
						_ = resp.Body.Close()
					}

					done <- err
				}
				badDone, goodDone := make(chan error, 1), make(chan error, 1)
				deadline := time.NewTimer(5 * time.Second)

				defer deadline.Stop()

				go do(badReq, badDone)

				if custom {
					select {
					case observed := <-classified:
						if !errorGraphWithin(observed) {
							t.Error("predicate received an unbounded error graph")
						} else if !errors.Is(observed, ErrUninspectableCause) {
							t.Error("predicate did not receive ErrUninspectableCause")
						}
					case <-deadline.C:
						t.Fatal("predicate was not invoked promptly")
					}
				} else {
					select {
					case <-started:
					case <-deadline.C:
						t.Fatal("transport was not invoked promptly")
					}
				}

				go do(goodReq, goodDone)

				select {
				case err := <-goodDone:
					if err != nil {
						t.Fatalf("concurrent request: %v", err)
					}
				case <-deadline.C:
					t.Fatal("concurrent request through the same breaker blocked")
				}

				select {
				case err := <-badDone:
					if !errors.Is(err, ErrUninspectableCause) {
						t.Fatal("request did not return ErrUninspectableCause")
					}
				case <-deadline.C:
					t.Fatal("request with an uninspectable cause did not return promptly")
				}

				counts := cb.getBreakerForHost("api.test").Counts()
				if counts.TotalFailures != 1 || counts.TotalSuccesses != 1 {
					t.Errorf("counts = %+v, want one failure and one success", counts)
				}
			})
		}
	}
}

func TestBreakerIsSuccessfulConcurrent(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	cause := &NonRetryableError{Err: errors.New("application refusal")}
	cb := newCircuitBreakerClient(*newOptions(
		WithMaxHostBreakers(2),
		WithCircuitBreakerSettings("configured.test", &CircuitBreakerSettings{}),
		WithBreakerIsSuccessful(func(err error) bool {
			calls.Add(1)
			return errors.Is(err, ErrNonRetryable)
		}),
	))
	rt := cb.newCircuitBreakerRoundTripper(httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, cause
	}))

	t.Run("requests", func(t *testing.T) {
		for i := range 20 {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				t.Parallel()

				host := fmt.Sprintf("host-%d.test", i)
				if i%2 == 0 {
					host = "configured.test"
				}

				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+host+"/", nil)
				require.NoError(t, err)
				_, err = rt.RoundTrip(req)
				require.ErrorIs(t, err, cause)
			})
		}
	})
	require.Equal(t, int64(20), calls.Load())
	require.Equal(t, []gobreaker.State{gobreaker.StateClosed, gobreaker.StateClosed, gobreaker.StateClosed, gobreaker.StateClosed}, cb.States())
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
