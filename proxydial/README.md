# proxydial

```go
import "github.com/altessa-s/go-atlas-net/proxydial"
```

Forward-proxy dialers for client connections that do **not** go through `net/http`: SMTP, SOAP, raw TCP, gRPC, or any other protocol that needs to
tunnel through a corporate or egress proxy.

## When to use what

| Layer        | Symbol                                                | Use when…                                                                  |
|--------------|-------------------------------------------------------|----------------------------------------------------------------------------|
| **High**     | `proxydial.FromURL(u, opts...)`                       | Proxy comes from a `*url.URL` (config, env, runtime override, resolver)    |
| **Low**      | `proxydial.HTTPConnect` / `proxydial.SOCKS5Dialer`    | One-shot persistent connection, custom retry policy, custom TLS handshake  |

## Quick example: SMTP via go-mail

```go
dialFunc, err := proxydial.FromURL(proxyURL)
if err != nil {
    return nil, fmt.Errorf("build smtp proxy dialer: %w", err)
}
mailOpts = append(mailOpts, mail.WithDialContextFunc(dialFunc))
client, err := mail.NewClient(host, mailOpts...)
```

## Quick example: SOAP via custom HTTP client

```go
proxyURL, _ := url.Parse(os.Getenv("HTTPS_PROXY"))
dial, err := proxydial.FromURL(proxyURL)
if err != nil { return err }

tr := &http.Transport{DialContext: dial}
client := &http.Client{Transport: tr}
```

## Wire flow by scheme

| Scheme              | Wire flow                                                               |
|---------------------|-------------------------------------------------------------------------|
| `http://`           | TCP + HTTP `CONNECT`                                                    |
| `https://`          | TCP + TLS-to-proxy + HTTP `CONNECT`                                     |
| `socks5/socks5h`    | `golang.org/x/net/proxy.SOCKS5` (TCP, no TLS to the proxy)              |

## Symbols

| Symbol                                    | Description                                                                                    |
|-------------------------------------------|------------------------------------------------------------------------------------------------|
| `FromURL`                                 | High-level — build a `DialContextFunc` from a parsed `*url.URL`                                |
| `HTTPConnect`                             | Low-level — open TCP+TLS to the proxy and send a `CONNECT` for `addr`                          |
| `MaxConnectResponseHeaderBytes`           | Bound of the CONNECT response status line and headers (64 KiB); beyond it `ErrConnectResponseTooLarge` |
| `SOCKS5Dialer`                            | Low-level — construct a SOCKS5 dialer for `proxyURL`                                           |
| `TLSConfig`                               | Clone the user's `*tls.Config` and fill `ServerName`/`MinVersion` from the URL when zero       |
| `WithDialer`                              | Override the underlying `*net.Dialer` for `FromURL`                                            |
| `WithProxyTLSConfig`                      | Override the `*tls.Config` for the TLS handshake to an `https://` proxy                        |
| `WithSSRFGuard`                           | Resolve the destination locally, check every address with an `AddrGuard`, CONNECT to the IP   |
| `WithResolver`                            | `Resolver` used by the SSRF guard (default `net.DefaultResolver`)                              |
| `AddrGuard`                               | `func(host string, dst netip.AddrPort) error`; `httpclient.EgressPolicy.Check` is one          |
| `Resolver`                                | `LookupNetIP(ctx, network, host)`; `*net.Resolver` satisfies it                                |
| `DefaultDialer`                           | `*net.Dialer` pre-configured with `DefaultDialTimeout` and `DefaultDialKeepAlive`              |
| `DefaultDialTimeout`                      | Default TCP connect timeout (30s)                                                              |
| `DefaultDialKeepAlive`                    | Default TCP keep-alive interval (30s)                                                          |
| `DialContextFunc`                         | Type alias for `func(ctx, network, addr) (net.Conn, error)` (matches `net.Dialer.DialContext`) |

## SSRF guard

Without a guard the proxy resolves the destination host, so a client-side address check never sees where the connection goes.
`WithSSRFGuard(guard)` resolves the host once on the client, passes every resolved address (unmapped, zone dropped) to the guard, and refuses the
dial without contacting the proxy when any address is rejected — a name answering with both public and private addresses is treated as rebinding,
not as a fallback. The proxy is then asked (`CONNECT` or SOCKS5) for an approved IP address instead of the name, so the address that was checked is the
address it connects to; approved addresses are tried in resolver order. The caller's protocol layer keeps the original host name: `net/http` sends it
as the Host header and TLS server name, and clients that set `tls.Config.ServerName` themselves (SMTP) keep it too. IP literals are checked without a
lookup. The guard's error is returned wrapped; with `httpclient.EgressPolicy.Check` it is an `*httpclient.SSRFError` matching
`httpclient.ErrSSRFBlocked`. Keep the proxy's own ACLs as an additional control.

```go
policy, err := httpclient.NewEgressPolicy(nil, []httpclient.EgressRule{{Host: "*.internal"}})
if err != nil {
    return err
}
dial, err := proxydial.FromURL(proxyURL, proxydial.WithSSRFGuard(policy.Check))
if err != nil {
    return err
}
tr := &http.Transport{DialContext: dial}
```

## Out of scope

- End-to-end testing against a real HTTPS proxy with custom CA — consumers cover that at their integration layer.
- Proxy auto-detection (PAC, WPAD) — callers supply a static URL or a `http.ProxyFromEnvironment`-style resolver.
