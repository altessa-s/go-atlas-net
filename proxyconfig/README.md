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
| url    | url             | HTTP, HTTPS, SOCKS5 or SOCKS5H with explicit port         |
| host   | host, port      | HTTP proxy with optional auth.username and auth.password |

Only fields used by the selected mode may be set. An auth block requires a username. Ports range from 1 to 65535. Host mode accepts an ASCII DNS
name or unbracketed, zone-free IP address. URL mode accepts an empty or root path and rejects credentials, queries and fragments. This incorporates
Argos's stricter URL and hostname rules. Atlas deployments using embedded credentials must move them to host-mode auth for HTTP proxies.
Authenticated HTTPS and SOCKS proxies need caller-side handling.

URLValue returns nil for nil config, empty mode and none. Inspect Mode when translating these cases to transport options. The returned URL contains
live credentials. Formatting Config and Auth hides credentials. Marshaling masks secret fields, so serialized diagnostic output cannot restore them.
