// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package limiters

import (
	"cmp"
	"net/http"

	"github.com/altessa-s/go-atlas-core/types/nilcheck"
)

// RoundTripper implements [net/http.RoundTripper] by checking a
// [RequestsLimiter] before forwarding each request to the next transport.
// When the limiter denies a request, RoundTrip returns immediately with an
// error and does not contact the server.
type RoundTripper struct {
	next    http.RoundTripper
	limiter RequestsLimiter
}

// NewRoundTripper creates a [RoundTripper] that calls limiter.Allow with the
// request hostname before delegating to next. If next is nil,
// [net/http.DefaultTransport] is used. A nil limiter (including a typed nil
// and the nil returned by [Chain] with no limiters) makes the RoundTripper a
// pass-through that forwards every request to next unchecked.
//
// Example:
//
//	limiter := &myRateLimiter{limit: 100}
//	rt := limiters.NewRoundTripper(http.DefaultTransport, limiter)
//	client := &http.Client{Transport: rt}
func NewRoundTripper(next http.RoundTripper, limiter RequestsLimiter) *RoundTripper {
	if nilcheck.IsNil(limiter) {
		limiter = nil
	}
	return &RoundTripper{
		next:    cmp.Or(next, http.DefaultTransport),
		limiter: limiter,
	}
}

// RoundTrip checks [RequestsLimiter.Allow] using the request hostname as key.
// If the limiter returns a non-nil error the request is rejected without
// reaching the network with a [*RateLimitError] wrapping that error (a
// limiter that already returns a *RateLimitError, for example with RetryAfter
// set, is passed through); otherwise the request is forwarded to the next
// [net/http.RoundTripper]. Without a limiter the request is forwarded as is.
func (rt *RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if rt.limiter == nil {
		return rt.next.RoundTrip(req)
	}
	host := req.URL.Hostname()
	if err := rt.limiter.Allow(req.Context(), host); err != nil {
		if IsRateLimitError(err) != nil {
			return nil, err
		}
		return nil, &RateLimitError{Host: host, Err: err}
	}

	return rt.next.RoundTrip(req)
}

// CloseIdleConnections forwards to the next transport when it supports it, so
// [net/http.Client.CloseIdleConnections] releases pooled connections.
func (rt *RoundTripper) CloseIdleConnections() {
	if c, ok := rt.next.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}
