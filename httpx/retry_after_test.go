// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package httpx_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/httpx"
)

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		value  string
		max    time.Duration
		want   time.Duration
		wantOK bool
	}{
		{"absent", "", 0, 0, false},
		{"seconds", "120", 0, 2 * time.Minute, true},
		{"zero seconds", "0", 0, 0, true},
		{"spaces", " 5 ", 0, 5 * time.Second, true},
		{"capped seconds", "3600", time.Minute, time.Minute, true},
		{"huge seconds", "99999999999999999999", time.Minute, time.Minute, true},
		{"negative", "-5", 0, 0, false},
		{"signed", "+5", 0, 0, false},
		{"fraction", "1.5", 0, 0, false},
		{"garbage", "soon", 0, 0, false},
		{"date ahead", now.Add(90 * time.Second).Format(http.TimeFormat), 0, 90 * time.Second, true},
		{"date ahead capped", now.Add(time.Hour).Format(http.TimeFormat), time.Minute, time.Minute, true},
		{"date past", now.Add(-time.Hour).Format(http.TimeFormat), 0, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := http.Header{}
			if tc.value != "" {
				h.Set("Retry-After", tc.value)
			}

			got, ok := httpx.ParseRetryAfter(h, now, tc.max)
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.want, got)
		})
	}
}

func FuzzParseRetryAfter(f *testing.F) {
	for _, value := range []string{"", "120", "-1", "99999999999999999999", "Sun, 18 Oct 2026 12:00:00 GMT"} {
		f.Add(value)
	}

	now := time.Unix(0, 0)

	f.Fuzz(func(t *testing.T, value string) {
		h := http.Header{httpx.HeaderRetryAfter: {value}}
		delay, ok := httpx.ParseRetryAfter(h, now, 0)
		capped, cappedOK := httpx.ParseRetryAfter(h, now, time.Minute)
		require.Equal(t, ok, cappedOK)
		require.GreaterOrEqual(t, delay, time.Duration(0))
		require.Equal(t, min(delay, time.Minute), capped)

		if !ok {
			require.Zero(t, delay)
		}
	})
}
