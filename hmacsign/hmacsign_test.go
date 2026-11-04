// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package hmacsign_test

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/hmacsign"
)

func fixedClock(t time.Time) hmacsign.Clock { return func() time.Time { return t } }

// mustSigner builds a Signer and fails the test on a construction error.
func mustSigner(t testing.TB, scheme hmacsign.Scheme, secret []byte, opts ...hmacsign.Option) *hmacsign.Signer {
	t.Helper()
	s, err := hmacsign.NewSigner(scheme, secret, opts...)
	require.NoError(t, err)
	return s
}

// mustVerifier builds a Verifier and fails the test on a construction error.
func mustVerifier(t testing.TB, scheme hmacsign.Scheme, secret []byte, opts ...hmacsign.Option) *hmacsign.Verifier {
	t.Helper()
	v, err := hmacsign.NewVerifier(scheme, secret, opts...)
	require.NoError(t, err)
	return v
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"event":"payment.succeeded","id":"evt_123"}`)
	secret := []byte("whsec_test_secret_0123")

	for _, tc := range []struct {
		name   string
		scheme hmacsign.Scheme
	}{
		{"github", hmacsign.GitHub()},
		{"stripe", hmacsign.Stripe()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			signer := mustSigner(t, tc.scheme, secret, hmacsign.WithClock(fixedClock(now)))
			verifier := mustVerifier(t, tc.scheme, secret, hmacsign.WithClock(fixedClock(now)))

			header := signer.Sign(body)
			require.Equal(t, tc.scheme.HeaderName(), signer.HeaderName())
			require.NoError(t, verifier.Verify(header, body))
		})
	}
}

func TestConstructorsRejectShortSecret(t *testing.T) {
	t.Parallel()

	long := []byte(strings.Repeat("k", hmacsign.MinSecretLength))
	for _, tc := range []struct {
		name   string
		secret []byte
		opts   []hmacsign.Option
	}{
		{"nil primary", nil, nil},
		{"empty primary", []byte{}, nil},
		{"short primary", []byte(strings.Repeat("k", hmacsign.MinSecretLength-1)), nil},
		{"short rotation secret", long, []hmacsign.Option{hmacsign.WithSecrets([]byte("short"))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := hmacsign.NewVerifier(hmacsign.GitHub(), tc.secret, tc.opts...)
			require.ErrorIs(t, err, hmacsign.ErrSecretTooShort)
			if tc.opts == nil {
				_, err = hmacsign.NewSigner(hmacsign.GitHub(), tc.secret)
				require.ErrorIs(t, err, hmacsign.ErrSecretTooShort)
			}
		})
	}

	_, err := hmacsign.NewSigner(hmacsign.GitHub(), long)
	require.NoError(t, err)
	_, err = hmacsign.NewVerifier(hmacsign.GitHub(), long, hmacsign.WithSecrets(nil, []byte{}))
	require.NoError(t, err, "empty rotation secrets are dropped, not rejected")
}

func TestVerifyRejectsTamperedBody(t *testing.T) {
	t.Parallel()

	secret := []byte("whsec_test_secret_0123")
	signer := mustSigner(t, hmacsign.GitHub(), secret)
	verifier := mustVerifier(t, hmacsign.GitHub(), secret)

	header := signer.Sign([]byte("original"))
	err := verifier.Verify(header, []byte("tampered"))
	require.ErrorIs(t, err, hmacsign.ErrSignatureMismatch)
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	t.Parallel()

	body := []byte("payload")
	header := mustSigner(t, hmacsign.GitHub(), []byte("right_secret_0123456")).Sign(body)
	err := mustVerifier(t, hmacsign.GitHub(), []byte("wrong_secret_0123456")).Verify(header, body)
	require.ErrorIs(t, err, hmacsign.ErrSignatureMismatch)
}

func TestVerifyMalformedHeader(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		scheme hmacsign.Scheme
		header string
	}{
		{"github_not_hex", hmacsign.GitHub(), "sha256=zzzz"},
		{"github_empty", hmacsign.GitHub(), ""},
		{"stripe_no_timestamp", hmacsign.Stripe(), "v1=abcd"},
		{"stripe_no_signature", hmacsign.Stripe(), "t=123"},
		{"stripe_bad_timestamp", hmacsign.Stripe(), "t=notanumber,v1=abcd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := mustVerifier(t, tc.scheme, []byte("s_secret_0123456789")).Verify(tc.header, []byte("body"))
			require.ErrorIs(t, err, hmacsign.ErrMalformedSignature)
		})
	}
}

func TestStripeTimestampTolerance(t *testing.T) {
	t.Parallel()

	signedAt := time.Unix(1_700_000_000, 0)
	body := []byte("event")
	secret := []byte("whsec_test_secret_0123")
	header := mustSigner(t, hmacsign.Stripe(), secret, hmacsign.WithClock(fixedClock(signedAt))).Sign(body)

	// Ten minutes later, outside the default five-minute window.
	late := mustVerifier(t, hmacsign.Stripe(), secret,
		hmacsign.WithClock(fixedClock(signedAt.Add(10*time.Minute))))
	require.ErrorIs(t, late.Verify(header, body), hmacsign.ErrTimestampOutOfTolerance)

	// A widened tolerance accepts it; the signature itself is still valid.
	wide := mustVerifier(t, hmacsign.Stripe(), secret,
		hmacsign.WithClock(fixedClock(signedAt.Add(10*time.Minute))),
		hmacsign.WithTolerance(time.Hour))
	require.NoError(t, wide.Verify(header, body))

	// Tolerance 0 disables the timestamp check entirely.
	off := mustVerifier(t, hmacsign.Stripe(), secret,
		hmacsign.WithClock(fixedClock(signedAt.Add(24*time.Hour))),
		hmacsign.WithTolerance(0))
	require.NoError(t, off.Verify(header, body))
}

func TestSecretRotation(t *testing.T) {
	t.Parallel()

	body := []byte("payload")
	oldSecret := []byte("whsec_old_secret_0123")
	newSecret := []byte("whsec_new_secret_0123")

	// Signed with the old secret; the verifier trusts both during rotation.
	header := mustSigner(t, hmacsign.GitHub(), oldSecret).Sign(body)
	verifier := mustVerifier(t, hmacsign.GitHub(), newSecret, hmacsign.WithSecrets(oldSecret))
	require.NoError(t, verifier.Verify(header, body))
}

func TestStripeTimestampBounds(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		at   time.Time
		ok   bool
	}{
		{"past boundary", now.Add(-hmacsign.DefaultTolerance), true},
		{"future boundary", now.Add(hmacsign.DefaultTolerance), true},
		{"past outside", now.Add(-hmacsign.DefaultTolerance - time.Second), false},
		{"future outside", now.Add(hmacsign.DefaultTolerance + time.Second), false},
		{"far future", time.Date(2500, 1, 1, 0, 0, 0, 0, time.UTC), false},
		{"far past", time.Date(1500, 1, 1, 0, 0, 0, 0, time.UTC), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			secret, body := []byte("secret_secret_0123"), []byte("event")
			signer := mustSigner(t, hmacsign.Stripe(), secret, hmacsign.WithClock(fixedClock(tc.at)))
			verifier := mustVerifier(t, hmacsign.Stripe(), secret, hmacsign.WithClock(fixedClock(now)))
			err := verifier.Verify(signer.Sign(body), body)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, hmacsign.ErrTimestampOutOfTolerance)
			}
		})
	}
}

// plainScheme is a minimal custom [hmacsign.Scheme]: a bare hex digest of the
// body in an "X-Signature" header, no timestamp. It exercises the exported seam.
type plainScheme struct{}

func (plainScheme) HeaderName() string                      { return "X-Signature" }
func (plainScheme) Message(_ time.Time, body []byte) []byte { return body }

func (plainScheme) FormatHeader(_ time.Time, digest []byte) string {
	return hex.EncodeToString(digest)
}

func (plainScheme) ParseHeader(header string) (time.Time, [][]byte, error) {
	raw, err := hex.DecodeString(header)
	if err != nil || len(raw) == 0 {
		return time.Time{}, nil, hmacsign.ErrMalformedSignature
	}
	return time.Time{}, [][]byte{raw}, nil
}

func TestCustomScheme(t *testing.T) {
	t.Parallel()

	var scheme hmacsign.Scheme = plainScheme{}
	body := []byte("payload")
	secret := []byte("s3cr3t_s3cr3t_0123")

	header := mustSigner(t, scheme, secret).Sign(body)
	require.Equal(t, "X-Signature", scheme.HeaderName())
	require.NoError(t, mustVerifier(t, scheme, secret).Verify(header, body))
	require.ErrorIs(t, mustVerifier(t, scheme, secret).Verify(header, []byte("tampered")),
		hmacsign.ErrSignatureMismatch)
}

// TestGitHubKnownAnswer pins interop against GitHub's documented example so a
// change to the message construction is caught immediately.
// https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries
func TestGitHubKnownAnswer(t *testing.T) {
	t.Parallel()

	secret := []byte("It's a Secret to Everybody")
	body := []byte("Hello, World!")
	const want = "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"

	require.Equal(t, want, mustSigner(t, hmacsign.GitHub(), secret).Sign(body))
	require.NoError(t, mustVerifier(t, hmacsign.GitHub(), secret).Verify(want, body))
}

func TestConstructorsOwnSecret(t *testing.T) {
	t.Parallel()
	secret := []byte("original secret value")
	body := []byte("payload")
	signer := mustSigner(t, hmacsign.GitHub(), secret)
	verifier := mustVerifier(t, hmacsign.GitHub(), secret)
	header := signer.Sign(body)
	clear(secret)
	require.Equal(t, header, signer.Sign(body))
	require.NoError(t, verifier.Verify(header, body))
}
