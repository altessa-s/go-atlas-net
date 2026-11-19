// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import (
	"maps"
	"net/http"
	"slices"
)

// setHeaders applies a batch of header entries (one value per exact key) to
// h in sorted key order, so the result never depends on map or iterator
// order. An ordinary entry replaces the header's values (Set): of keys that
// differ only in case, the later in key order wins. Idempotency-Key and
// X-Idempotency-Key entries replace any earlier value of their header too,
// but all their case variants in the batch are kept (Add), so the client
// refuses them as ambiguous instead of silently sending one.
func setHeaders(h http.Header, entries map[string]string) {
	keys := slices.Sorted(maps.Keys(entries))

	for _, k := range keys {
		if isIdempotencyKeyName(k) {
			h.Del(k)
		}
	}

	for _, k := range keys {
		if isIdempotencyKeyName(k) {
			h.Add(k, entries[k])
			continue
		}

		h.Set(k, entries[k])
	}
}

// isIdempotencyKeyName reports whether name is Idempotency-Key or
// X-Idempotency-Key, ASCII case-insensitively.
func isIdempotencyKeyName(name string) bool {
	return asciiEqualFold(name, headerIdempotencyKey) || asciiEqualFold(name, headerXIdempotencyKey)
}
