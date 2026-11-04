// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package hmacsign

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"github.com/altessa-s/go-atlas-core/types/nilcheck"
)

// Signer produces the signature header for an outbound request body under a
// [Scheme] and a secret. It is safe for concurrent use.
type Signer struct {
	scheme Scheme
	secret []byte
	opts   *options
}

// MinSecretLength is the shortest secret, in bytes, [NewSigner] and
// [NewVerifier] accept. An empty or short HMAC key lets anyone who can guess it
// forge a valid signature, so a secret that never resolved (an unset env var
// expanding to "") fails construction instead of silently signing with "".
const MinSecretLength = 16

// NewSigner builds a Signer that signs with secret using scheme. It returns
// [ErrSecretTooShort] when secret is shorter than [MinSecretLength]. The signer
// owns a copy of secret.
func NewSigner(scheme Scheme, secret []byte, opts ...Option) (*Signer, error) {
	if err := checkSecret(secret); err != nil {
		return nil, err
	}

	return &Signer{scheme: scheme, secret: bytes.Clone(secret), opts: newOptions(opts...)}, nil
}

// checkSecret rejects a secret shorter than [MinSecretLength].
func checkSecret(secret []byte) error {
	if len(secret) < MinSecretLength {
		return fmt.Errorf("%w: got %d bytes, need at least %d", ErrSecretTooShort, len(secret), MinSecretLength)
	}

	return nil
}

// HeaderName is the HTTP header the produced signature belongs in, a convenience
// forwarding to the scheme so callers need not hold it separately.
func (s *Signer) HeaderName() string { return s.scheme.HeaderName() }

// Sign returns the signature header value for body. For a timestamped scheme it
// stamps the current time (from the configured clock) into the signature.
func (s *Signer) Sign(body []byte) string {
	now := s.opts.clock()
	return s.scheme.FormatHeader(now, digest(s.secret, s.scheme.Message(now, body)))
}

// Verifier authenticates an inbound request body against the signature header a
// sender produced under the same [Scheme] and a shared secret. It is safe for
// concurrent use.
type Verifier struct {
	scheme  Scheme
	secrets [][]byte
	opts    *options
}

// NewVerifier builds a Verifier for scheme accepting secret. Additional secrets
// for rotation are supplied with [WithSecrets]; the timestamp tolerance (for a
// timestamped scheme) with [WithTolerance]. It returns [ErrSecretTooShort] when
// the primary secret or any rotation secret is shorter than [MinSecretLength].
// The verifier owns copies of the secrets.
func NewVerifier(scheme Scheme, secret []byte, opts ...Option) (*Verifier, error) {
	o := newOptions(opts...)
	if nilcheck.IsNil(o.replayCache) {
		// A typed nil cache would pass the nil checks and panic on use.
		o.replayCache = nil
	}

	if o.replayCache != nil && o.tolerance > math.MaxInt64/replayWindowSpan {
		return nil, ErrToleranceTooLarge
	}

	secrets := make([][]byte, 0, 1+len(o.secrets))
	secrets = append(secrets, bytes.Clone(secret))

	secrets = append(secrets, o.secrets...)
	for _, s := range secrets {
		if err := checkSecret(s); err != nil {
			return nil, err
		}
	}

	return &Verifier{scheme: scheme, secrets: secrets, opts: o}, nil
}

// HeaderName is the HTTP header the signature is read from, forwarding to the
// scheme.
func (v *Verifier) HeaderName() string { return v.scheme.HeaderName() }

// Verify authenticates body against the signature header. It returns nil when a
// configured secret produces a digest matching the header (compared in constant
// time). It returns [ErrMalformedSignature] for an unparsable header,
// [ErrTimestampOutOfTolerance] when a timestamped scheme's timestamp falls
// outside the tolerance window, [ErrSignatureMismatch] when the header is
// well-formed but authentic to no configured secret, and [ErrReplayed] when a
// [ReplayCache] is configured and already accepted this signature. Pass the
// exact bytes that were signed.
//
// Without a [ReplayCache] a timestamped signature can be replayed for as long
// as it stays inside the tolerance window, and a scheme without a timestamp
// (GitHub) can be replayed indefinitely: it guarantees integrity and origin
// only. Deduplicate such deliveries by the provider's delivery id.
func (v *Verifier) Verify(header string, body []byte) error {
	signedAt, sigs, err := v.scheme.ParseHeader(header)
	if err != nil {
		return err
	}

	timed := !signedAt.IsZero() && v.opts.tolerance > 0
	if timed {
		if err := v.checkTimestamp(signedAt); err != nil {
			return err
		}
	}

	// Compare every digest against every secret without stopping at the first
	// match, so the verification time does not reveal which secret signed the
	// delivery (the primary or a rotation secret).
	msg := v.scheme.Message(signedAt, body)
	matched := false

	for _, secret := range v.secrets {
		expected := digest(secret, msg)
		for _, sig := range sigs {
			matched = hmac.Equal(expected, sig) || matched
		}
	}

	if !matched {
		return ErrSignatureMismatch
	}

	if timed && v.opts.replayCache != nil {
		return v.checkReplay(msg)
	}

	return nil
}

// replayKey returns the replay-cache key of msg within namespace.
func replayKey(namespace string, msg []byte) [sha256.Size]byte {
	if namespace == "" {
		return sha256.Sum256(msg)
	}

	h := sha256.New()
	_ = binary.Write(h, binary.BigEndian, uint64(len(namespace))) // a hash.Hash write never fails
	_, _ = h.Write([]byte(namespace))
	_, _ = h.Write(msg)

	var key [sha256.Size]byte
	h.Sum(key[:0])

	return key
}

// replayWindowSpan is the replay-cache TTL in tolerances: a signature is
// accepted from tolerance before to tolerance after its timestamp.
const replayWindowSpan = 2

// checkReplay records the accepted signed message in the replay cache for the
// full width of the tolerance window and rejects one already recorded. A cache
// error fails closed.
//
// The key is the SHA-256 of the replay namespace and the signed message
// (timestamp and body), not the matched digest: during a secret rotation the
// sender attaches one digest per secret, and a digest-keyed cache would accept
// the same delivery once more for every other secret it verifies under. The
// namespace is length-prefixed so that no namespace and message pair hashes
// like another; an empty namespace keys on the message alone.
func (v *Verifier) checkReplay(msg []byte) error {
	key := replayKey(v.opts.replayNamespace, msg)

	seen, err := v.opts.replayCache.CheckAndStore(hex.EncodeToString(key[:]), replayWindowSpan*v.opts.tolerance)
	if err != nil {
		return fmt.Errorf("hmacsign: replay cache: %w", err)
	}

	if seen {
		return ErrReplayed
	}

	return nil
}

// checkTimestamp rejects a signed timestamp further than the tolerance from now,
// in either direction (a future timestamp is as suspect as a stale one).
func (v *Verifier) checkTimestamp(ts time.Time) error {
	now := v.opts.clock()
	if ts.Before(now.Add(-v.opts.tolerance)) || ts.After(now.Add(v.opts.tolerance)) {
		return fmt.Errorf("%w: signed at %s", ErrTimestampOutOfTolerance, ts)
	}

	return nil
}
