// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package limiters

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/altessa-s/go-atlas-net/httpx"
)

type noopLimiter struct{}

func (l *noopLimiter) Allow(_ context.Context, _ string) error { return nil }

func BenchmarkRoundTripper_RoundTrip(b *testing.B) {
	next := httpx.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK}, nil
	})
	rt := NewRoundTripper(next, &noopLimiter{})
	req, _ := http.NewRequest("GET", "http://example.com/test", nil)

	for b.Loop() {
		_, _ = rt.RoundTrip(req)
	}
}

func BenchmarkRateLimitError_Error(b *testing.B) {
	err := &RateLimitError{Host: "api.test", RetryAfter: 5 * time.Second}
	for b.Loop() {
		_ = err.Error()
	}
}

func BenchmarkChain_Allow(b *testing.B) {
	for _, n := range []int{1, 2, 4, 8} {
		ls := make([]RequestsLimiter, n)
		for i := range ls {
			ls[i] = &noopLimiter{}
		}
		chained := Chain(ls...)
		b.Run("limiters_"+strconv.Itoa(n), func(b *testing.B) {
			ctx := b.Context()
			b.ReportAllocs()
			for b.Loop() {
				_ = chained.Allow(ctx, "api.example.com")
			}
		})
	}
}
