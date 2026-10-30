// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package client

import (
	"maps"
	"slices"
	"time"

	"github.com/sony/gobreaker/v2"
)

// Observer receives the client's telemetry events: completed requests,
// retries, circuit breaker rejections and breaker state transitions. Set it
// with [WithObserver]; the default discards every event. An Observer serves
// one client: [New] calls Attach once, before the first request, and the
// other methods from the request path, concurrently, so they must be safe for
// concurrent use and cheap.
//
// The client itself records no metrics and reports no health: an Observer
// turns these events into metrics and health checks.
//
// Embed [NoopObserver] to implement only the events of interest and stay
// source-compatible when events are added.
type Observer interface {
	// Attach hands the observer a read-only view of the client's circuit
	// breakers. It is called once by [New], before any other method.
	Attach(breakers Breakers)
	// RequestDone reports a request that completed, successfully or not,
	// after all its retry attempts.
	RequestDone(e RequestEvent)
	// Retry reports a retry about to be made, before its backoff sleep.
	Retry(e AttemptEvent)
	// BreakerRejected reports an attempt rejected by an open (or saturated
	// half-open) circuit breaker.
	BreakerRejected(e AttemptEvent)
	// BreakerStateChanged reports a circuit breaker state transition. It runs
	// inside the breaker's state-change callback, with the breaker's lock
	// held: it must not call [Breakers] methods synchronously (do it from
	// another goroutine) or block.
	BreakerStateChanged(e BreakerStateEvent)
}

// RequestEvent describes a request completed by the retry layer.
type RequestEvent struct {
	// Method is the request method.
	Method string
	// Host is the request URL's host name, without port.
	Host string
	// StatusClass is the status class of the last reply ("2xx", "3xx",
	// "4xx", "5xx") or "unknown" when no reply was received.
	StatusClass string
	// Retries is the number of retries made (zero for a single attempt).
	Retries int
	// Failed reports that the request ended with an error after all its
	// attempts (a transport error, an exhausted retryable status or a
	// non-retryable failure), before any [ErrorHandler] ran.
	Failed bool
	// Duration is the time spent in the retry layer, retries and backoff
	// included.
	Duration time.Duration
}

// AttemptEvent identifies the request a retry or a breaker rejection belongs to.
type AttemptEvent struct {
	// Method is the request method.
	Method string
	// Host is the request URL's host name, without port.
	Host string
}

// BreakerStateEvent describes a circuit breaker state transition.
type BreakerStateEvent struct {
	// Name is the breaker's name.
	Name string
	// Host is the host the breaker serves: the request URL's host (with port
	// when the URL has one), or empty for the client-wide breaker shared by
	// hosts beyond [WithMaxHostBreakers].
	Host string
	// From is the previous state.
	From gobreaker.State
	// To is the new state.
	To gobreaker.State
}

// Breakers is a read-only view of a client's circuit breakers, handed to
// [Observer.Attach]. Its methods are safe for concurrent use but must not be
// called from [Observer.BreakerStateChanged] synchronously.
type Breakers interface {
	// States returns the current state of every breaker in service: the
	// per-host breakers, plus the client-wide breaker once it has served a
	// request or while no per-host breaker exists.
	States() []gobreaker.State
	// HostState returns the state of host's breaker, or
	// [gobreaker.StateClosed] when host has none yet.
	HostState(host string) gobreaker.State
	// ConfiguredHosts returns the hosts with explicit
	// [CircuitBreakerSettings], sorted.
	ConfiguredHosts() []string
}

// NoopObserver is an [Observer] that discards every event. It is the
// client's default; embed it to implement a subset of the events.
type NoopObserver struct{}

// Attach implements [Observer].
func (NoopObserver) Attach(Breakers) {}

// RequestDone implements [Observer].
func (NoopObserver) RequestDone(RequestEvent) {}

// Retry implements [Observer].
func (NoopObserver) Retry(AttemptEvent) {}

// BreakerRejected implements [Observer].
func (NoopObserver) BreakerRejected(AttemptEvent) {}

// BreakerStateChanged implements [Observer].
func (NoopObserver) BreakerStateChanged(BreakerStateEvent) {}

// defaultObserver is the default of [WithObserver].
func defaultObserver() Observer { return NoopObserver{} }

// States implements [Breakers].
func (c *circuitBreakerClient) States() []gobreaker.State {
	// Snapshot under the lock and query State() after releasing it: State()
	// may run OnStateChange synchronously, and a user callback that issues a
	// request to a new host needs the write lock.
	c.mu.RLock()
	breakers := slices.Collect(maps.Values(c.hostBreakers)) //nolint:bodyclose // breaker pointers, no response
	c.mu.RUnlock()
	// The client-wide breaker only serves hosts beyond the per-host cap; an
	// idle one must not mask an all-open set of per-host breakers.
	if c.sharedUsed.Load() || len(breakers) == 0 {
		breakers = append(breakers, c.CircuitBreaker) //nolint:bodyclose // breaker pointer, no response
	}
	states := make([]gobreaker.State, len(breakers))
	for i, b := range breakers {
		states[i] = b.State()
	}
	return states
}

// HostState implements [Breakers].
func (c *circuitBreakerClient) HostState(host string) gobreaker.State {
	c.mu.RLock()
	breaker, exists := c.hostBreakers[host]
	c.mu.RUnlock()
	if !exists {
		return gobreaker.StateClosed
	}
	return breaker.State()
}

// ConfiguredHosts implements [Breakers].
func (c *circuitBreakerClient) ConfiguredHosts() []string {
	return slices.Sorted(maps.Keys(c.hostBreakerSettings))
}
