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
