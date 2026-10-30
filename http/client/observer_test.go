// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package client_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"
	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/httpx"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

// recordingObserver records every event it receives.
type recordingObserver struct {
	mu       sync.Mutex
	breakers httpclient.Breakers
	done     []httpclient.RequestEvent
	retries  []httpclient.AttemptEvent
	rejected []httpclient.AttemptEvent
	states   []httpclient.BreakerStateEvent
}

func (o *recordingObserver) Attach(b httpclient.Breakers) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.breakers = b
}

func (o *recordingObserver) RequestDone(e httpclient.RequestEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.done = append(o.done, e)
}

func (o *recordingObserver) Retry(e httpclient.AttemptEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.retries = append(o.retries, e)
}

func (o *recordingObserver) BreakerRejected(e httpclient.AttemptEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.rejected = append(o.rejected, e)
}

func (o *recordingObserver) BreakerStateChanged(e httpclient.BreakerStateEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.states = append(o.states, e)
}

func statusTransport(status int) http.RoundTripper {
	return httpx.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	})
}

func observeGet(t *testing.T, c *http.Client, url string) error {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := c.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	return err
}

func TestObserver_AttachAndRequestDone(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	c := httpclient.New(
		httpclient.WithoutSSRFProtection(),
		httpclient.WithClient(&http.Client{Transport: statusTransport(http.StatusOK)}),
		httpclient.WithObserver(obs),
		httpclient.WithCircuitBreakerSettings("api.test", &httpclient.CircuitBreakerSettings{}),
	)
	require.NotNil(t, obs.breakers, "New must attach the breaker view")
	require.Equal(t, []string{"api.test"}, obs.breakers.ConfiguredHosts())

	require.NoError(t, observeGet(t, c, "http://api.test:8080/x"))

	require.Len(t, obs.done, 1)
	e := obs.done[0]
	require.Equal(t, http.MethodGet, e.Method)
	require.Equal(t, "api.test", e.Host)
	require.Equal(t, "2xx", e.StatusClass)
	require.Zero(t, e.Retries)
	require.False(t, e.Failed)
	require.Positive(t, e.Duration)
	require.Empty(t, obs.retries)
}

func TestObserver_RetriesAndFailure(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	c := httpclient.New(
		httpclient.WithoutSSRFProtection(),
		httpclient.WithClient(&http.Client{Transport: statusTransport(http.StatusServiceUnavailable)}),
		httpclient.WithObserver(obs),
		httpclient.WithRetryMax(2),
		httpclient.WithRetryWait(time.Millisecond, time.Millisecond),
		// Keep the breaker out of the way: one failure ratio trip would stop retries.
		httpclient.WithCircuitBreakerSettings("api.test", &httpclient.CircuitBreakerSettings{
			ReadyToTrip: func(gobreaker.Counts) bool { return false },
		}),
	)

	require.Error(t, observeGet(t, c, "http://api.test/x"))

	require.Len(t, obs.retries, 2)
	require.Equal(t, httpclient.AttemptEvent{Method: http.MethodGet, Host: "api.test"}, obs.retries[0])
	require.Len(t, obs.done, 1)
	require.Equal(t, "5xx", obs.done[0].StatusClass)
	require.Equal(t, 2, obs.done[0].Retries)
	require.True(t, obs.done[0].Failed)
}

func TestObserver_BreakerEvents(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	c := httpclient.New(
		httpclient.WithoutSSRFProtection(),
		httpclient.WithClient(&http.Client{Transport: httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		})}),
		httpclient.WithObserver(obs),
		httpclient.WithRetryMax(0),
		httpclient.WithBreakerTimeout(time.Hour),
	)

	for range httpclient.DefaultBreakerMinRequests {
		require.Error(t, observeGet(t, c, "http://down.test/"))
	}
	err := observeGet(t, c, "http://down.test/")
	require.True(t, httpclient.IsCircuitBreakerOpen(err))

	obs.mu.Lock()
	defer obs.mu.Unlock()
	require.Equal(t, []httpclient.AttemptEvent{{Method: http.MethodGet, Host: "down.test"}}, obs.rejected)
	require.Len(t, obs.states, 1)
	require.Equal(t, "down.test", obs.states[0].Host)
	require.Equal(t, gobreaker.StateClosed, obs.states[0].From)
	require.Equal(t, gobreaker.StateOpen, obs.states[0].To)
	require.Equal(t, []gobreaker.State{gobreaker.StateOpen}, obs.breakers.States())
}
