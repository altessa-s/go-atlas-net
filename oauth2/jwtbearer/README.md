# jwtbearer

```go
import "github.com/altessa-s/go-atlas-net/oauth2/jwtbearer"
```

Exchange RFC 7523 assertions through an injected [httpx.Doer](../../httpx). New accepts an RSA crypto.Signer. ParsePrivateKey reads unencrypted PKCS#1
and PKCS#8 PEM. The assertion contains iss, aud, iat and a one-hour exp.

## Options

| Option               | Default                  | Behavior                                                                   |
|----------------------|--------------------------|----------------------------------------------------------------------------|
| WithScopes           | Empty                    | Space-delimited scope claim                                                |
| WithSubject          | Empty                    | Preserve the delegated subject in sub and legacy Google prn                |
| WithKeyID            | Empty                    | Preserve the optional kid header verbatim                                  |
| WithAudience         | Endpoint URL             | Override aud independently of the HTTP destination                         |
| WithClock            | clock.Real()             | Time used for assertions, token expiry and cache checks                    |
| WithExpiryMargin     | 10 seconds               | Early refresh capped at half the lifetime, zero allowed                    |
| WithOptionalExpiry   | Off                      | Accept absent or zero expires_in and return an uncached token              |
| WithMaxLifetime      | 0 (uncapped)             | Cap the issued token lifetime, independently of the assertion lifetime     |
| WithMaxResponseBytes | MaxResponseBytes (1 MiB) | Limit successful and unsuccessful response bodies to a positive byte count |
| WithInsecureHTTP     | Off                      | Allow a caller-approved plain HTTP endpoint                                |

## Usage

Keep one Source per credential. Token coalesces cache misses. The shared request carries the first waiter's context values and is canceled when the last
waiter leaves. Fetch bypasses the cache and posts directly on the supplied context. Exchange posts a caller-signed assertion and supports external
signing algorithms. Custom Doers must honor cancellation and disable redirects. The package disables redirects on injected http.Client values. Keep
retries, metrics, endpoint trust policy and multi-credential eviction in the consumer.

The HTTPS endpoint may include a query. Both the POST and the default aud claim preserve the endpoint query. The issuer must be a non-empty UTF-8
string. The issuer, subject and key ID retain their whitespace.

Token preserves access_token, refresh_token, id_token, token_type, issued_token_type, scope and expiry. The decoder ignores unknown fields. A positive
integral expires_in may be a JSON number or a quoted integer. The lifetime saturates at the maximum time.Duration or the configured WithMaxLifetime cap,
whichever is smaller.

## Errors

Transport and body-read failures match ErrRequest. Use errors.Is or errors.As to inspect the original cause, including cancellation, redirect refusal
and typed client errors. ResponseError provides the HTTP status and the number of bytes observed, up to the configured limit plus one. On a rejected
HTTP status, its Body contains at most the configured limit. ErrorCode, ErrorDescription and ErrorURI expose RFC 6749 fields from a complete JSON error
response. These provider values are redacted strings. The returned error text excludes the endpoint URL, assertion, bodies and underlying diagnostic
text. Unwrapped errors retain the caller's original data.
