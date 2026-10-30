// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package netipx

import (
	"net/netip"
	"testing"
)

func BenchmarkIsPrivate(b *testing.B) {
	addr := netip.MustParseAddr("10.0.0.1")
	for b.Loop() {
		IsPrivate(addr)
	}
}

func BenchmarkIsPrivate_Public(b *testing.B) {
	addr := netip.MustParseAddr("8.8.8.8")
	for b.Loop() {
		IsPrivate(addr)
	}
}
