# proxyconfig

```go
import "github.com/altessa-s/go-atlas-net/proxyconfig"
```

Decode YAML or JSON proxy settings into Config. Validate checks the same rules as URLValue, which returns an explicit proxy suitable for
[proxydial](../proxydial) or the [HTTP client](../http/client).

## Modes

| Mode   | Required fields | Transport behavior                                      |
|--------|-----------------|---------------------------------------------------------|
| Empty  | None            | Preserve the caller's transport defaults                |
| none   | None            | Explicitly disable proxy resolution                     |
| url    | url             | HTTP, HTTPS, SOCKS5 or SOCKS5H with explicit port       |
| host   | host, port      | HTTP proxy with optional auth.username and auth.password |

Only fields used by the selected mode may be set. Both modes require an explicit port from 1 to 65535.

URL mode accepts optional userinfo. Percent-encode `@` and `/` in credentials. An ambiguous `@` is rejected. The path may be empty or `/`. Empty query
and fragment delimiters (`?`, `#`, `?#`) are accepted. The returned URL preserves an empty query and drops an empty fragment, following `net/url`.

Host mode accepts resolver-specific names, IPv4 and unbracketed IPv6, including scoped addresses such as `fe80::1%eth0`. Supply the port separately.
Empty hosts, whitespace, control characters, `/` and `@` are rejected. An optional auth block requires a username and may include a password.

URLValue returns nil for nil config, empty mode and none. Inspect Mode when translating these cases to transport options. The returned URL contains live
credentials for the transport. Formatting Config and Auth hides credentials. Marshaling masks secret fields and validation errors omit the URL.
