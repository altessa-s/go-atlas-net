// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package hmacsign

import "errors"

var (
	// ErrMalformedSignature indicates the signature header could not be parsed:
	// a missing digest, an unparsable hex value, or (for schemes that sign a
	// timestamp) a missing or non-numeric timestamp field.
	ErrMalformedSignature = errors.New("hmacsign: malformed signature header")

	// ErrSignatureMismatch indicates the header was well-formed but no configured
	// secret produced a matching HMAC — the body is not authentic (or was signed
	// with an unknown key). Match with errors.Is.
	ErrSignatureMismatch = errors.New("hmacsign: signature mismatch")

	// ErrTimestampOutOfTolerance indicates a timestamped scheme carried a
	// timestamp outside the allowed window ([WithTolerance]), the signal of a
	// replayed (or badly clock-skewed) request.
	ErrTimestampOutOfTolerance = errors.New("hmacsign: timestamp outside tolerance")

	// ErrSecretTooShort indicates [NewSigner] or [NewVerifier] was given a
	// secret (primary or rotation) shorter than [MinSecretLength]. It is a
	// construction-time error: a missing secret must not yield a signer or
	// verifier keyed by a guessable value.
	ErrSecretTooShort = errors.New("hmacsign: secret too short")

	// ErrReplayed indicates an authentic signature that the configured
	// [ReplayCache] had already accepted inside the tolerance window: the same
	// request was delivered twice.
	ErrReplayed = errors.New("hmacsign: signature replayed")
)
