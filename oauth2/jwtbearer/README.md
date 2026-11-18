# jwtbearer

```go
import "github.com/altessa-s/go-atlas-net/oauth2/jwtbearer"
```

Exchange RFC 7523 assertions through an injected [httpx.Doer](../../httpx). New accepts an RSA crypto.Signer. ParsePrivateKey reads unencrypted
PKCS#1 and PKCS#8 PEM. The assertion contains iss, aud, iat and a one-hour exp.

## Options

| Option           | Default       | Behavior                                                |
|------------------|---------------|---------------------------------------------------------|
| WithScopes       | Empty         | Space-delimited scope claim                             |
| WithSubject      | Empty         | Delegated sub and legacy Google prn claims              |
| WithKeyID        | Empty         | Optional kid header                                     |
| WithAudience     | Endpoint URL  | Override aud independently of the HTTP destination      |
| WithClock        | clock.Real()  | Time used for assertions, expiry and cache checks       |
| WithExpiryMargin | 10 seconds    | Early refresh capped at half the lifetime, zero allowed |
| WithOptionalExpiry | Off         | Accept absent or zero expires_in and return an uncached token   |
| WithInsecureHTTP | Off           | Allow a caller-approved plain HTTP endpoint             |

## Usage

Keep one Source per credential. Token coalesces cache misses. The shared request carries the first waiter's context values and is canceled when the
last waiter leaves. Fetch bypasses the cache and posts directly on the supplied context. Exchange posts a caller-signed assertion and supports
external signing algorithms. Custom Doers must honor cancellation and disable redirects. The package disables redirects on injected http.Client
values. Keep retries, metrics, endpoint trust policy and multi-credential eviction in the consumer.

Token preserves access_token, refresh_token, id_token, token_type, issued_token_type, scope and expiry. A positive integral expires_in can be a JSON number
or a quoted integer. ResponseError carries the HTTP status and observed byte count. The request and response body, assertion, endpoint URL and
underlying transport or signing error are excluded from errors.
