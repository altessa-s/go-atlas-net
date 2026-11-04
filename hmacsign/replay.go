// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package hmacsign

import (
	"sync"
	"time"
)

// ReplayCache remembers signatures a [Verifier] has already accepted so a
// captured request cannot be replayed inside the tolerance window. Install one
// with [WithReplayCache]; a deployment with several receivers needs a shared
// implementation (Redis SET NX with a TTL, for example), while
// [NewMemoryReplayCache] covers a single process.
type ReplayCache interface {
	// CheckAndStore atomically records key for ttl and reports whether key was
	// already recorded and unexpired. A non-nil error makes [Verifier.Verify]
	// fail closed.
	CheckAndStore(key string, ttl time.Duration) (seen bool, err error)
}

// MemoryReplayCache is an in-process [ReplayCache]. Expired entries are swept
// on insertion. It is safe for concurrent use.
type MemoryReplayCache struct {
	mu        sync.Mutex
	entries   map[string]time.Time
	nextSweep time.Time
}

// NewMemoryReplayCache returns an empty in-process [ReplayCache].
func NewMemoryReplayCache() *MemoryReplayCache {
	return &MemoryReplayCache{entries: make(map[string]time.Time)}
}

// CheckAndStore implements [ReplayCache].
func (c *MemoryReplayCache) CheckAndStore(key string, ttl time.Duration) (bool, error) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.After(c.nextSweep) {
		for k, exp := range c.entries {
			if !now.Before(exp) {
				delete(c.entries, k)
			}
		}
		c.nextSweep = now.Add(ttl)
	}
	if exp, ok := c.entries[key]; ok && now.Before(exp) {
		return true, nil
	}
	c.entries[key] = now.Add(ttl)
	return false, nil
}
