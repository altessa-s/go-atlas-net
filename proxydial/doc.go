// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

// Package proxydial provides forward-proxy dialers for client
// connections that do not go through net/http: SMTP, SOAP, raw TCP,
// gRPC, and any other protocol that needs to tunnel through a
// corporate or egress proxy.
//
// # Layered API
//
//   - [FromURL] — high-level: build a [DialContextFunc] from a
//     parsed *url.URL. Use when the proxy comes from configuration,
//     the environment, a runtime override or a custom resolver.
//
//   - [HTTPConnect] / [SOCKS5Dialer] — low-level escape hatches: open
//     a single tunneled connection. Use when you control the
//     lifecycle yourself (one-shot persistent connection, custom
//     retry policy, custom TLS handshake).
//
// # Wire flow by scheme
//
//   - http://         — plain TCP + HTTP CONNECT
//   - https://        — TCP + TLS-to-proxy + HTTP CONNECT
//   - socks5/socks5h  — golang.org/x/net/proxy.SOCKS5 (TCP, no proxy TLS)
//
// # SSRF guard
//
// A proxy normally resolves the destination host itself, so a client-side
// address check never sees where the connection goes. [WithSSRFGuard] moves
// resolution to the client: the dialer from [FromURL] resolves the host once
// (through [WithResolver], default [net.DefaultResolver]), passes every
// resolved address to the [AddrGuard], refuses the dial before contacting the
// proxy when any address is rejected, and asks the proxy (CONNECT or SOCKS5)
// for an approved IP address instead of the name. The address that is checked
// is the address the proxy connects to. The original host name stays with the
// caller's protocol layer (Host header, TLS server name).
//
//	policy, err := httpclient.NewEgressPolicy(nil, []httpclient.EgressRule{{Host: "*.internal"}})
//	if err != nil {
//	    return err
//	}
//	dial, err := proxydial.FromURL(proxyURL, proxydial.WithSSRFGuard(policy.Check))
//
// With httpclient.EgressPolicy.Check as the guard, refusals are
// *httpclient.SSRFError values matching httpclient.ErrSSRFBlocked.
//
// # Why this exists
//
// [github.com/altessa-s/go-atlas-net/http/client] accepts a proxy
// configuration via its options (`WithProxyURL`, `WithProxy`,
// `WithProxyTLSConfig`) and delegates the "open TCP, optionally wrap
// with TLS, send CONNECT" wire flow to this package. Consumers that
// need raw-TCP-through-proxy (SMTP, SOAP, gRPC, etc.) use [FromURL]
// directly.
//
// # Out of scope
//
//   - End-to-end testing against a real HTTPS proxy with custom CA —
//     consumers cover that at their own integration layer.
//   - Proxy auto-detection (PAC, WPAD) — callers supply a static URL
//     or a [http.ProxyFromEnvironment]-style resolver.
package proxydial
