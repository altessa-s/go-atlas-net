// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package httpx

import (
	"net/http"
	"testing"
)

func BenchmarkHTTPHeaderGetter_GetSingleHeader(b *testing.B) {
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "abc123")
	g := NewHTTPHeaderGetter(req)

	for b.Loop() {
		g.GetSingleHeader("X-Request-ID")
	}
}

func BenchmarkHTTPHeaderGetter_GetHeader(b *testing.B) {
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Add("X-Forwarded-For", "1.2.3.4")
	req.Header.Add("X-Forwarded-For", "5.6.7.8")
	g := NewHTTPHeaderGetter(req)

	for b.Loop() {
		g.GetHeader("X-Forwarded-For")
	}
}
