// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package proxydial

import (
	"context"
	"fmt"
	"net"
	"net/url"
)

// FromURL builds a [DialContextFunc] that tunnels every dial through
// proxyURL. Dispatches on proxyURL.Scheme:
//
//   - http, https        — [HTTPConnect]
//   - socks5, socks5h    — [SOCKS5Dialer]
//
// Other schemes return an error so misconfiguration surfaces at
// dialer construction time rather than at the first request.
//
// A nil proxyURL returns (nil, nil) — caller should treat that as
// "use a direct dial" and skip wiring a custom dialer at all.
//
// Pass zero or more [Option] values (e.g. [WithDialer],
// [WithProxyTLSConfig]) to override defaults; options compose
// left-to-right and a later one overrides an earlier one.
//
// With [WithSSRFGuard] the returned dialer resolves the destination host
// once, refuses the dial before contacting the proxy when the guard rejects
// any resolved address, and asks the proxy (CONNECT or SOCKS5) for an
// approved IP address instead of the host name. The caller's protocol layer
// still uses the original host name: an [net/http.Transport] keeps it in the
// Host header and as the TLS server name, and so does any client that
// configures its TLS ServerName itself. Use it whenever the destination is
// not trusted; the proxy's own ACLs remain an additional control.
//
// Dial and handshake errors name the proxy host:port (proxyURL.Host), never
// its user info. Pass only a URL whose credentials sit in a well-formed user
// info: an unencoded "/" or "@" in a password
// moves part of it into the host, where it would reach DNS and error text.
func FromURL(proxyURL *url.URL, opts ...Option) (DialContextFunc, error) {
	if proxyURL == nil {
		return nil, nil //nolint:nilnil
	}

	o := newOptions(opts...)

	var dial DialContextFunc
	switch proxyURL.Scheme {
	case "http", "https":
		dialer := o.dialer
		tlsCfg := o.proxyTLSConfig
		u := proxyURL
		dial = func(ctx context.Context, _, addr string) (net.Conn, error) {
			return HTTPConnect(ctx, dialer, u, tlsCfg, addr)
		}

	case "socks5", "socks5h":
		var err error
		if dial, err = SOCKS5Dialer(proxyURL, o.dialer); err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q (allowed: http, https, socks5, socks5h)", proxyURL.Scheme)
	}

	if o.ssrfGuard != nil {
		dial = guardDial(dial, o.ssrfGuard, o.resolver)
	}
	return dial, nil
}
