// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package transportpool

//go:generate optgen generate --type=options

import "time"

// DefaultMaxEntries bounds the number of cached transports.
const DefaultMaxEntries = 64

// DefaultRetireDelay is how long after the last response of an evicted
// transport finishes its idle connections are closed one more time.
const DefaultRetireDelay = time.Second

type options struct {
	// maxEntries bounds the LRU cache. Nonpositive values use DefaultMaxEntries.
	maxEntries int `optgen:"default=DefaultMaxEntries"`
	// retireDelay is the delay of the final cleanup of an evicted transport,
	// counted from the end of its last response. A transport may release a
	// connection only after the response body was closed (HTTP/2 cleans a
	// stream up asynchronously), so the idle connections are closed once more
	// after this delay. Nonpositive values use DefaultRetireDelay.
	retireDelay time.Duration `optgen:"default=DefaultRetireDelay"`
}
