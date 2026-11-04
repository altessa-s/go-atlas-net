// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package hmacsign

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestMemoryReplayCacheSweepsShortEntriesBehindLongOnes pins that the sweep is
// scheduled for the earliest expiry: a long-TTL entry stored first must not
// keep an expired short-TTL entry alive.
func TestMemoryReplayCacheSweepsShortEntriesBehindLongOnes(t *testing.T) {
	t.Parallel()

	c := NewMemoryReplayCache()
	_, err := c.CheckAndStore("long", time.Hour)
	require.NoError(t, err)
	_, err = c.CheckAndStore("short", time.Millisecond)
	require.NoError(t, err)

	time.Sleep(5 * time.Millisecond)
	_, err = c.CheckAndStore("next", time.Hour)
	require.NoError(t, err)

	c.mu.Lock()
	defer c.mu.Unlock()
	require.NotContains(t, c.entries, "short", "the expired short-TTL entry was not swept")
	require.Len(t, c.entries, 2)
}
