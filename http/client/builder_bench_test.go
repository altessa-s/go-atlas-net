// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import (
	"net/http"
	"net/url"
	"testing"
)

func BenchmarkRequestBuilder_BuildWithContext(b *testing.B) {
	rb := &requestBuilder{
		method:      http.MethodGet,
		url:         "https://api.example.com/v1/orders?page=2",
		headers:     map[string]string{"Accept": "application/json"},
		queryParams: url.Values{"limit": {"50"}},
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := rb.BuildWithContext(b.Context()); err != nil {
			b.Fatal(err)
		}
	}
}
