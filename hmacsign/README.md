# hmacsign

```go
import "github.com/altessa-s/go-atlas-net/hmacsign"
```

Signs and verifies HTTP request bodies with a shared-secret HMAC — the scheme webhook providers such as Stripe and GitHub use to prove a request came
from them and was not tampered with. It is the body-signing counterpart to the token packages ([`auth/oidc`](https://github.com/altessa-s/go-atlas/tree/develop/auth/oidc),
[`auth/jwt`](https://github.com/altessa-s/go-atlas/tree/develop/auth/jwt)): those authenticate a bearer *token*, this authenticates the *payload*.

A `Verifier` authenticates an inbound webhook; a `Signer` produces the header for an outbound one. Both use
HMAC-SHA256; the `Verifier` compares signatures in constant time (`hmac.Equal`).

`NewSigner` and `NewVerifier` return `ErrSecretTooShort` when the primary secret or any rotation secret is shorter than `MinSecretLength` (16 bytes):
an empty or short key — usually a secret reference that never resolved — would let anyone forge a signature, so construction fails closed.

## Schemes

A `Scheme` is one provider's wire format. Two are built in:

| Constructor  | Header                | Value                  | Signed message | Timestamp |
|--------------|-----------------------|------------------------|----------------|-----------|
| `GitHub()`   | `X-Hub-Signature-256` | `sha256=<hex>`         | raw body       | no        |
| `Stripe()`   | `Stripe-Signature`    | `t=<unix>,v1=<hex>`    | `<t>.<body>`   | yes       |

Stripe binds a timestamp into the signature, so a stale (replayed) request is rejected by the tolerance window.

## Replay protection

The tolerance window alone still admits a captured Stripe request replayed within it. `WithReplayCache` closes that gap: an accepted delivery is
recorded for twice the tolerance (the full `±tolerance` window) and a second delivery fails with `ErrReplayed`. The cache key is the SHA-256 of the
signed message (timestamp and body), not the matched digest, so during a secret rotation a resend carrying only the old secret's digest is still a
replay. `NewMemoryReplayCache()` covers a single process; several receivers need a shared `ReplayCache` (Redis `SET NX` with a TTL, for example). A
cache error fails closed. Receivers that share one cache but verify different endpoints or secrets set distinct `WithReplayNamespace` values;
without one, the same body and timestamp delivered to two of them would be rejected as a replay by the second.

The GitHub scheme signs no timestamp, so it is **integrity-only**: a captured request verifies forever and the replay cache is not consulted.
Deduplicate GitHub deliveries by the `X-GitHub-Delivery` id in the application.

`Scheme` is an exported interface — implement `HeaderName`, `Message`, `FormatHeader`, and `ParseHeader` for a provider with a different grammar
(Shopify's base64 body signature, say). A `ParseHeader` that returns the zero time turns the replay window off; the digest algorithm and secret stay
owned by the signer/verifier, so a custom scheme only describes the wire format.

## Options

| Option                       | Applies to        | Description                                                                             |
|------------------------------|-------------------|-----------------------------------------------------------------------------------------|
| `WithSecrets(...[]byte)`     | `Verifier`        | Additional accepted secrets for zero-downtime rotation. Empty secrets are dropped; a non-empty one under `MinSecretLength` fails `NewVerifier`. |
| `WithTolerance(time.Duration)` | `Verifier`      | Replay window for a timestamped scheme. Default 5m; `0` disables the timestamp check.    |
| `WithClock(Clock)`           | both              | Override the time source (tests pin it; signing stamps it). Default `time.Now`.          |
| `WithReplayCache(ReplayCache)` | `Verifier`      | Reject a second delivery of an accepted signature (`ErrReplayed`). Timestamped schemes only. Default off. With a cache, a tolerance above half the largest `time.Duration` fails with `ErrToleranceTooLarge`. |
| `WithReplayNamespace(string)`  | `Verifier`      | Scope the replay-cache keys of one receiver sharing a cache with others. Default empty (no scope). |

## Usage

Verify an inbound Stripe webhook:

```go
v, err := hmacsign.NewVerifier(hmacsign.Stripe(), webhookSecret)
if err != nil {
    return err // ErrSecretTooShort for a secret under MinSecretLength
}

body, _ := io.ReadAll(r.Body) // read the raw bytes once
if err := v.Verify(r.Header.Get(v.HeaderName()), body); err != nil {
    http.Error(w, "bad signature", http.StatusUnauthorized)
    return
}
```

Sign an outbound webhook:

```go
s, err := hmacsign.NewSigner(hmacsign.Stripe(), webhookSecret)
if err != nil {
    return err
}
req.Header.Set(s.HeaderName(), s.Sign(body))
```

Rotate a secret without downtime — verify against the new and old secret at once:

```go
v, err := hmacsign.NewVerifier(hmacsign.GitHub(), newSecret, hmacsign.WithSecrets(oldSecret))
```

Pass the **exact** bytes that were signed: the HMAC is over the raw body, so any re-encoding (re-marshaling parsed JSON, for example) breaks the
signature.

## Errors

| Sentinel                     | Meaning                                                                              |
|------------------------------|--------------------------------------------------------------------------------------|
| `ErrMalformedSignature`      | The header could not be parsed (missing `sha256=` prefix, missing or empty digest, bad hex, missing/non-numeric `t`). |
| `ErrTimestampOutOfTolerance` | A timestamped scheme's timestamp fell outside the tolerance window (likely a replay). |
| `ErrSignatureMismatch`       | The header was well-formed but authentic to no configured secret.                    |
| `ErrSecretTooShort`          | `NewSigner`/`NewVerifier` got a secret shorter than `MinSecretLength` (construction). |
| `ErrReplayed`                | The `ReplayCache` had already accepted this signature (a duplicate delivery).        |
| `ErrToleranceTooLarge`       | `NewVerifier`: a replay cache is set and twice the tolerance overflows `time.Duration`. |

Match with `errors.Is`.

## HTTP, config, and factory

- **[`httpsign`](httpsign)** — the `net/http` adapter: `httpsign.Middleware(v)` reads the body once, verifies it, restores it for the handler, and
  rejects unauthentic requests (401). Kept in its own package so this one never imports `net/http`.
- **[`factory`](https://github.com/altessa-s/go-atlas/tree/develop/factory/ext/net/hmacsign)** + **[`webhookconfig.Signature`](https://github.com/altessa-s/go-atlas/tree/develop/config/webhook)** — build a `Verifier` or `Signer` from a YAML
  template (`scheme`, `secret`, `additionalSecrets`, `tolerance`) with the secret sourced through `redacted.RedactedString`.

## See also

- [`auth/jwt`](https://github.com/altessa-s/go-atlas/tree/develop/auth/jwt) — mint and verify bearer tokens (authenticates the caller, not the body).
- [go-kryptos](https://github.com/altessa-s/go-kryptos) — source the webhook secret from a manager with rotation.
