// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package transportpool

//go:generate optgen generate --type=options

// DefaultMaxEntries bounds the number of cached transports.
const DefaultMaxEntries = 64

type options struct {
	// maxEntries bounds the LRU cache. Nonpositive values use DefaultMaxEntries.
	maxEntries int `optgen:"default=DefaultMaxEntries"`
}
