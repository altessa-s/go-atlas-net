// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package httpx_test

import (
	"testing"

	"github.com/altessa-s/go-atlas-net/httpx"
)

func BenchmarkParseBearer(b *testing.B) {
	values := []string{"Bearer aZ09-._~+/=="}

	b.ReportAllocs()

	for b.Loop() {
		_, _ = httpx.ParseBearer(values)
	}
}
