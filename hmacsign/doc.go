// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

// Package hmacsign signs and verifies HTTP request bodies with a shared-secret
// HMAC, the scheme webhook providers such as Stripe and GitHub use to prove a
// request came from them and was not tampered with in transit.
//
// It is the body-signing counterpart to the token packages: auth/oidc and
// auth/jwt authenticate a bearer *token*, this authenticates the *payload*. A
// receiver verifies an inbound webhook; a sender signs an outbound one.
//
// # Schemes
//
// A [Scheme] encodes one provider's wire format: the header the signature lives
// in, how the signed message is built from the timestamp and body, and how the
// header value is parsed and formatted. Two are built in:
//
//   - [GitHub] — header "X-Hub-Signature-256", value "sha256=<hex>", HMAC over
//     the raw body. No timestamp.
//   - [Stripe] — header "Stripe-Signature", value "t=<unix>,v1=<hex>", HMAC over
//     "<t>.<body>". The signed timestamp enables replay protection.
//
// Both use HMAC-SHA256 (the modern variant of each provider); legacy SHA-1 is
// out of scope.
//
// # Verifying
//
//	v, err := hmacsign.NewVerifier(hmacsign.Stripe(), secret)
//	if err != nil {
//	    return err // hmacsign.ErrSecretTooShort
//	}
//	if err := v.Verify(r.Header.Get(v.HeaderName()), body); err != nil {
//	    http.Error(w, "bad signature", http.StatusUnauthorized)
//	    return
//	}
//
// [Verifier.Verify] compares in constant time ([hmac.Equal]), rejects a Stripe
// timestamp outside the tolerance window ([WithTolerance], default five minutes)
// to blunt replays, and accepts any of several secrets ([WithSecrets]) so a key
// can be rotated without downtime.
//
// # Replay protection
//
// The tolerance window bounds, but does not prevent, replay: a captured Stripe
// request verifies again until its timestamp leaves the window. [WithReplayCache]
// records each accepted signature for twice the tolerance and rejects a second
// delivery with [ErrReplayed]; [NewMemoryReplayCache] serves one process, a
// fleet needs a shared [ReplayCache]. The [GitHub] scheme signs no timestamp,
// so it is integrity-only: replay protection there must come from the
// application deduplicating the provider's delivery id.
//
// # Secrets
//
// [NewSigner] and [NewVerifier] return [ErrSecretTooShort] for a primary or
// rotation secret shorter than [MinSecretLength] (16 bytes). An HMAC keyed by
// "" or a short guessable value lets anyone forge a signature, and the usual
// cause is a secret reference that never resolved, so construction fails
// closed instead of signing with it.
//
// # Signing
//
//	s, err := hmacsign.NewSigner(hmacsign.Stripe(), secret)
//	if err != nil {
//	    return err
//	}
//	req.Header.Set(s.HeaderName(), s.Sign(body))
//
// Read the raw body once and pass the same bytes to Verify/Sign: the HMAC is
// over the exact bytes, so any re-encoding breaks the signature.
package hmacsign
