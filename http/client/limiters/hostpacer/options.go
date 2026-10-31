// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package hostpacer

//go:generate optgen generate --type=options

import "github.com/altessa-s/go-atlas-core/time/clock"

// DefaultMaxHosts is the soft bound on tracked hosts above which [Pacer]
// drops the buckets that have refilled completely; see [WithMaxHosts].
const DefaultMaxHosts = 1024

// options holds the tunables of a [Pacer]; the interval and burst are
// positional arguments of [New].
type options struct {
	// maxHosts is a soft bound on the number of tracked hosts: at or above
	// it, a new host first triggers a sweep that drops the fully refilled
	// buckets. Dropping such a bucket is lossless, as a fresh one starts full
	// too. Buckets still refilling are kept, so the pacer can exceed maxHosts
	// by the hosts used within one burst × interval window. LRU eviction is
	// deliberately not used: dropping a bucket that is still refilling would
	// hand its host a fresh full bucket and let it exceed its pace.
	maxHosts int `optgen:"default=DefaultMaxHosts"`
	// clock is the time source bucket levels are read at and the waits for
	// a slot run on.
	clock clock.Clock `optgen:"default=clock.Real()"`
}
