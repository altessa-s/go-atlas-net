// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package hmacsign_test

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/hmacsign"
)

const replaySecret = "whsec_replay_secret_01"

func TestReplayCacheRejectsSecondDelivery(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	body := []byte("event")
	header := mustSigner(t, hmacsign.Stripe(), []byte(replaySecret), hmacsign.WithClock(fixedClock(now))).Sign(body)
	v := mustVerifier(t, hmacsign.Stripe(), []byte(replaySecret),
		hmacsign.WithClock(fixedClock(now)),
		hmacsign.WithReplayCache(hmacsign.NewMemoryReplayCache()))

	require.NoError(t, v.Verify(header, body))
	require.ErrorIs(t, v.Verify(header, body), hmacsign.ErrReplayed)

	// A fresh delivery (new timestamp, so a new signature) is still accepted.
	later := mustSigner(t, hmacsign.Stripe(), []byte(replaySecret),
		hmacsign.WithClock(fixedClock(now.Add(time.Second)))).Sign(body)
	require.NoError(t, v.Verify(later, body))
}

func TestReplayCacheRejectsReplayUnderRotatedSecret(t *testing.T) {
	t.Parallel()

	const newSecret, oldSecret = "whsec_rotation_new_01", "whsec_rotation_old_01"

	now := time.Unix(1_700_000_000, 0)
	body := []byte("event")
	sigNew := mustSigner(t, hmacsign.Stripe(), []byte(newSecret), hmacsign.WithClock(fixedClock(now))).Sign(body)
	sigOld := mustSigner(t, hmacsign.Stripe(), []byte(oldSecret), hmacsign.WithClock(fixedClock(now))).Sign(body)
	v := mustVerifier(t, hmacsign.Stripe(), []byte(newSecret),
		hmacsign.WithSecrets([]byte(oldSecret)),
		hmacsign.WithClock(fixedClock(now)),
		hmacsign.WithReplayCache(hmacsign.NewMemoryReplayCache()))

	// During rotation the sender signs with both secrets: "t=..,v1=new,v1=old".
	both := sigNew + "," + strings.TrimPrefix(sigOld, fmt.Sprintf("t=%d,", now.Unix()))
	require.NoError(t, v.Verify(both, body))

	// Resending the same delivery with only one of the digests is still a replay.
	require.ErrorIs(t, v.Verify(sigOld, body), hmacsign.ErrReplayed)
	require.ErrorIs(t, v.Verify(sigNew, body), hmacsign.ErrReplayed)
}

func TestReplayCacheIgnoresUnauthenticHeaders(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	cache := &recordingCache{}
	v := mustVerifier(t, hmacsign.Stripe(), []byte(replaySecret),
		hmacsign.WithClock(fixedClock(now)), hmacsign.WithReplayCache(cache))

	forged := mustSigner(t, hmacsign.Stripe(), []byte("attacker_secret_012345"),
		hmacsign.WithClock(fixedClock(now))).Sign([]byte("event"))
	require.ErrorIs(t, v.Verify(forged, []byte("event")), hmacsign.ErrSignatureMismatch)
	require.Zero(t, cache.calls, "an unauthentic header must not reach the cache")
}

func TestReplayCacheTTLCoversToleranceWindow(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	cache := &recordingCache{}
	v := mustVerifier(t, hmacsign.Stripe(), []byte(replaySecret),
		hmacsign.WithClock(fixedClock(now)), hmacsign.WithTolerance(time.Minute), hmacsign.WithReplayCache(cache))

	header := mustSigner(t, hmacsign.Stripe(), []byte(replaySecret), hmacsign.WithClock(fixedClock(now))).Sign([]byte("e"))
	require.NoError(t, v.Verify(header, []byte("e")))
	require.Equal(t, 1, cache.calls)
	require.Equal(t, 2*time.Minute, cache.ttl, "an entry must outlive the whole +/- tolerance window")
}

func TestReplayCacheSkippedWithoutTimestamp(t *testing.T) {
	t.Parallel()

	cache := &recordingCache{}

	for _, tc := range []struct {
		name   string
		scheme hmacsign.Scheme
		opts   []hmacsign.Option
	}{
		{"github has no timestamp", hmacsign.GitHub(), nil},
		{"tolerance disabled", hmacsign.Stripe(), []hmacsign.Option{hmacsign.WithTolerance(0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte("event")
			header := mustSigner(t, tc.scheme, []byte(replaySecret)).Sign(body)
			opts := append([]hmacsign.Option{hmacsign.WithReplayCache(cache)}, tc.opts...)
			v := mustVerifier(t, tc.scheme, []byte(replaySecret), opts...)
			require.NoError(t, v.Verify(header, body))
			require.NoError(t, v.Verify(header, body))
		})
	}

	require.Zero(t, cache.calls)
}

func TestReplayCacheErrorFailsClosed(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	boom := errors.New("cache down")
	v := mustVerifier(t, hmacsign.Stripe(), []byte(replaySecret),
		hmacsign.WithClock(fixedClock(now)), hmacsign.WithReplayCache(&recordingCache{err: boom}))

	header := mustSigner(t, hmacsign.Stripe(), []byte(replaySecret), hmacsign.WithClock(fixedClock(now))).Sign([]byte("e"))
	require.ErrorIs(t, v.Verify(header, []byte("e")), boom)
}

func TestMemoryReplayCacheExpires(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		c := hmacsign.NewMemoryReplayCache()

		seen, err := c.CheckAndStore("k", time.Minute)
		require.NoError(t, err)
		require.False(t, seen)

		seen, err = c.CheckAndStore("k", time.Minute)
		require.NoError(t, err)
		require.True(t, seen)

		time.Sleep(time.Minute + time.Second)

		seen, err = c.CheckAndStore("k", time.Minute)
		require.NoError(t, err)
		require.False(t, seen, "an expired entry must not count as a replay")
	})
}

// recordingCache counts calls and reports nothing as seen.
type recordingCache struct {
	calls int
	ttl   time.Duration
	err   error
}

func (c *recordingCache) CheckAndStore(_ string, ttl time.Duration) (bool, error) {
	c.calls++
	c.ttl = ttl

	return false, c.err
}

func TestNewVerifierRejectsToleranceTooLargeForReplayCache(t *testing.T) {
	t.Parallel()

	huge := time.Duration(math.MaxInt64)/2 + 1
	_, err := hmacsign.NewVerifier(hmacsign.Stripe(), []byte(replaySecret),
		hmacsign.WithTolerance(huge), hmacsign.WithReplayCache(hmacsign.NewMemoryReplayCache()))
	require.ErrorIs(t, err, hmacsign.ErrToleranceTooLarge)

	_, err = hmacsign.NewVerifier(hmacsign.Stripe(), []byte(replaySecret), hmacsign.WithTolerance(huge))
	require.NoError(t, err, "without a replay cache the tolerance is not limited")
}

func TestNewVerifierTreatsTypedNilReplayCacheAsAbsent(t *testing.T) {
	t.Parallel()

	var cache *hmacsign.MemoryReplayCache

	v, err := hmacsign.NewVerifier(hmacsign.Stripe(), []byte(replaySecret), hmacsign.WithReplayCache(cache))
	require.NoError(t, err)

	body := []byte(`{"id":"evt_1"}`)
	header := mustSigner(t, hmacsign.Stripe(), []byte(replaySecret)).Sign(body)
	require.NotPanics(t, func() { require.NoError(t, v.Verify(header, body)) })
}
