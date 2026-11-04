// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package hmacsign

//go:generate optgen generate

import (
	"bytes"
	"time"

	"github.com/altessa-s/go-atlas-core/collections/slices"
)

// DefaultTolerance is the replay window applied to a timestamped scheme's
// signature; Stripe uses the same five-minute default. A zero tolerance
// ([WithTolerance] with 0) disables the timestamp check.
const DefaultTolerance = 5 * time.Minute

// Clock returns the current time. It is an injectable seam so tests can pin the
// wall clock; production uses [time.Now].
type Clock func() time.Time

// defaultClock is the production wall clock used for signing timestamps and
// tolerance comparisons.
var defaultClock Clock = time.Now

// options carries the signer/verifier tunables. clock is an injectable seam so
// tests can pin the wall clock.
type options struct {
	clock     Clock         `optgen:"default=defaultClock"`
	tolerance time.Duration `optgen:"default=DefaultTolerance" optval:"positive=allow_zero"`

	// replayCache, when set, rejects a second delivery of an already accepted
	// signature ([ErrReplayed]). It applies only to a timestamped scheme with a
	// non-zero tolerance: entries live for twice the tolerance, the width of
	// the acceptance window. A scheme without a timestamp (GitHub) is
	// integrity-only and never consults it. Nil (the default) disables it.
	// With a cache set, [NewVerifier] rejects a tolerance above half the
	// largest time.Duration ([ErrToleranceTooLarge]).
	replayCache ReplayCache

	// secrets holds additional accepted secrets beyond the primary one passed to
	// [NewVerifier], enabling zero-downtime rotation. Set via [WithSecrets]; the
	// verifier accepts a body signed by any of them. Signer-only fields ignore it.
	secrets [][]byte `opt:"-"`
}

// WithSecrets adds accepted secrets beyond the primary one, so a webhook secret
// can be rotated without downtime: [Verifier.Verify] accepts a body signed by
// the primary secret or any of these. Empty or nil secrets are dropped; a call
// with none is ignored. It has no effect on a [Signer], which always signs with
// its single primary secret.
func WithSecrets(secrets ...[]byte) Option {
	return func(o *options) {
		for _, s := range secrets {
			o.secrets = slices.AppendIf(o.secrets, len(s) > 0, bytes.Clone(s))
		}
	}
}
