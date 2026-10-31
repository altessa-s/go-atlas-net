// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client_test

import (
	"net/http"
	"testing"
	"time"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

func BenchmarkParseRetryAfter(b *testing.B) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		value string
	}{
		{"delta_seconds", "120"},
		{"http_date", now.Add(30 * time.Second).Format(http.TimeFormat)},
		{"past_date", now.Add(-time.Hour).Format(http.TimeFormat)},
		{"malformed", "soon"},
		{"absent", ""},
	}
	for _, tc := range cases {
		h := http.Header{}
		if tc.value != "" {
			h.Set("Retry-After", tc.value)
		}
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = httpclient.ParseRetryAfter(h, now, time.Minute)
			}
		})
	}
}
