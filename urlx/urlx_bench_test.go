// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package urlx_test

import (
	"testing"

	"github.com/altessa-s/go-atlas-net/urlx"
)

func BenchmarkParseBaseURL(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := urlx.ParseBaseURL("https://api.example.org/v1/"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkJoinPath(b *testing.B) {
	base, err := urlx.ParseBaseURL("https://api.example.org/v1")
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := urlx.JoinPath(base, "chat/completions"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCanonical(b *testing.B) {
	base, err := urlx.ParseBaseURL("HTTPS://API.Example.org/v1")
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	for b.Loop() {
		_ = urlx.Canonical(base)
	}
}

// BenchmarkJoinPath_Shapes covers the element shapes clients pass: several
// plain segments, segments that need escaping, a deep path and a base whose
// path is already escaped.
func BenchmarkJoinPath_Shapes(b *testing.B) {
	cases := []struct {
		name string
		base string
		elem []string
	}{
		{"multi_elements", "https://api.example.org/v1", []string{"projects", "p-123", "files", "f-456"}},
		{"escaped_segments", "https://api.example.org/v1", []string{"files", "report 2026?.pdf", "rev#3%"}},
		{"deep_single_element", "https://api.example.org/v1", []string{"a/b/c/d/e/f/g/h/i/j/k/l"}},
		{"escaped_base", "https://api.example.org/tenant%2Fone/", []string{"items", "42/"}},
		{"unicode", "https://api.example.org/v1", []string{"städte", "東京"}},
	}
	for _, tc := range cases {
		base, err := urlx.ParseBaseURL(tc.base)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := urlx.JoinPath(base, tc.elem...); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
