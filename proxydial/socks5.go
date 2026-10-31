// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package proxydial

import (
	"context"
	"fmt"
	"net"
	"net/url"

	xproxy "golang.org/x/net/proxy"
)

// SOCKS5Dialer constructs a SOCKS5 dialer for proxyURL and returns a
// per-connection dial function. dialer controls TCP options for the
// connection to the SOCKS proxy itself. The returned func always
// dials over TCP — SOCKS5 supports only TCP for the inner conn.
//
// SOCKS does not TLS-wrap the proxy connection; callers passing a
// proxy *tls.Config alongside SOCKS should be aware that it has no
// effect here.
//
// Note: golang.org/x/net/proxy.SOCKS5 always sends ATYP=DomainName
// for hostname addresses, performing DNS on the proxy side. The
// "socks5" and "socks5h" URL schemes are therefore treated as
// synonyms by callers of this dialer — there is no client-side DNS
// resolution mode. Callers needing client-side DNS must resolve to
// an IP literal before passing addr.
func SOCKS5Dialer(proxyURL *url.URL, dialer *net.Dialer) (DialContextFunc, error) {
	var auth *xproxy.Auth
	if u := proxyURL.User; u != nil {
		pwd, _ := u.Password()
		auth = &xproxy.Auth{User: u.Username(), Password: pwd}
	}
	d, err := xproxy.SOCKS5("tcp", proxyURL.Host, auth, dialer)
	if err != nil {
		return nil, fmt.Errorf("build socks5 dialer for %s: %w", proxyURL.Host, err)
	}
	contextDialer, ok := d.(xproxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("socks5 dialer for %s does not implement ContextDialer", proxyURL.Host)
	}
	return func(ctx context.Context, _, addr string) (net.Conn, error) {
		return contextDialer.DialContext(ctx, "tcp", addr)
	}, nil
}
