// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package httpx

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ParseRetryAfter reads the Retry-After header of h (RFC 9110 §10.2.3): a
// non-negative number of seconds or an HTTP date, relative to now. A date in
// the past yields zero. A positive maxDelay caps the result; a non-positive
// one leaves it uncapped. ok is false when the header is absent or malformed
// (including a negative number), in which case the duration is zero.
func ParseRetryAfter(h http.Header, now time.Time, maxDelay time.Duration) (d time.Duration, ok bool) {
	v := strings.TrimSpace(h.Get(HeaderRetryAfter))
	if v == "" {
		return 0, false
	}

	if isDigits(v) {
		secs, err := strconv.ParseInt(v, 10, 64)
		if err != nil || secs > int64(math.MaxInt64/time.Second) {
			d = time.Duration(math.MaxInt64) // out of range: as long as allowed
		} else {
			d = time.Duration(secs) * time.Second
		}
	} else {
		at, err := http.ParseTime(v)
		if err != nil {
			return 0, false
		}

		d = max(at.Sub(now), 0)
	}

	if maxDelay > 0 {
		d = min(d, maxDelay)
	}

	return d, true
}

// isDigits reports whether s is a non-empty run of ASCII digits.
func isDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}

	return s != ""
}
