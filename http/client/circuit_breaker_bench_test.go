// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import "testing"

func BenchmarkGetBreakerForHost(b *testing.B) {
	newCB := func(opt ...Option) *circuitBreakerClient {
		return newCircuitBreakerClient(*newOptions(opt...))
	}

	b.Run("cached", func(b *testing.B) {
		cb := newCB()
		cb.getBreakerForHost("a.test")
		b.ReportAllocs()
		for b.Loop() {
			cb.getBreakerForHost("a.test")
		}
	})

	b.Run("overflow", func(b *testing.B) {
		cb := newCB(WithMaxHostBreakers(0))
		b.ReportAllocs()
		for b.Loop() {
			cb.getBreakerForHost("a.test")
		}
	})
}
