// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package proxydial

//go:generate optgen generate --type=options

import (
	"crypto/tls"
	"net"
)

// options holds the internal configuration state consumed by [FromURL].
// Configure through the generated [Option] values; the struct is never
// exposed to callers.
type options struct {
	// dialer overrides the underlying *net.Dialer used to reach the
	// proxy. Defaults to [DefaultDialer]. Generated WithDialer skips
	// nil arguments so callers can pass through configuration values
	// unconditionally.
	dialer *net.Dialer `optgen:"default=DefaultDialer()"`

	// proxyTLSConfig is the *tls.Config used for the TLS handshake to
	// an https:// proxy. The caller's config is cloned via [TLSConfig]
	// before use; it is never mutated. ServerName and MinVersion are
	// filled in from the proxy URL only when left at the zero value.
	// No effect for http:// or socks5:// proxies.
	proxyTLSConfig *tls.Config

	// ssrfGuard, set by the generated WithSSRFGuard, makes the dialer
	// returned by [FromURL] resolve the destination host itself before the
	// proxy is asked to connect: every resolved address must pass the guard,
	// otherwise the dial fails with the guard's error and the proxy is never
	// contacted. The proxy is then asked for an approved IP address instead
	// of the host name, so it cannot resolve the name to another address.
	// Without a guard the proxy resolves the destination. Pass
	// httpclient.EgressPolicy.Check for the go-atlas private-address and
	// egress-rule checks.
	ssrfGuard AddrGuard `opt:"SSRFGuard"`

	// resolver looks up destination host names for ssrfGuard. Defaults to
	// [net.DefaultResolver].
	resolver Resolver `optgen:"notnil,default=net.DefaultResolver"`
}
