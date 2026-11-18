// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import (
	"net/http"
	"slices"
	"time"
)

// DefaultRetryAfterMax caps how long a server's Retry-After may stall a retry
// of the client's retry layer (see [WithRetryAfterMax]).
const DefaultRetryAfterMax = time.Minute

// DefaultRetryAfterStatuses returns the reply statuses whose Retry-After the
// retry layer honors by default: 429 Too Many Requests and 503 Service
// Unavailable (see [WithRetryAfterStatuses]).
func DefaultRetryAfterStatuses() []int {
	return []int{http.StatusTooManyRequests, http.StatusServiceUnavailable}
}

// honorsRetryAfter reports whether the Retry-After of a reply with status is
// honored under statuses.
func honorsRetryAfter(statuses []int, status int) bool {
	return slices.Contains(statuses, status)
}
