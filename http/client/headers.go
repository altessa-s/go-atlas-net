// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import (
	"iter"
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

// setHeaderSeq applies seq to h in iteration order: an ordinary entry replaces
// the header's values (Set), so the later of two case variants wins. The
// Idempotency-Key and X-Idempotency-Key entries (one value per exact key, the
// later winning) replace every earlier value of their header, under any ASCII
// case of its name, and are all kept (Add), so the client refuses case
// variants within the batch as ambiguous.
func setHeaderSeq(h http.Header, seq iter.Seq2[string, string]) {
	var (
		keys  []string
		value = make(map[string]string)
	)

	for k, v := range seq {
		if !isIdempotencyKeyName(k) {
			h.Set(k, v)
			continue
		}

		if _, seen := value[k]; !seen {
			keys = append(keys, k)
		}

		value[k] = v
	}

	// The batch replaces every earlier value of an idempotency header it
	// sets, under any case of its name, before adding its own variants.
	for name := range h {
		for _, k := range keys {
			if asciiEqualFold(name, k) {
				delete(h, name)
				break
			}
		}
	}

	for _, k := range keys {
		h.Add(k, value[k])
	}
}

// isIdempotencyKeyName reports whether name is Idempotency-Key or
// X-Idempotency-Key, ASCII case-insensitively.
func isIdempotencyKeyName(name string) bool {
	return asciiEqualFold(name, headerIdempotencyKey) || asciiEqualFold(name, headerXIdempotencyKey)
}
