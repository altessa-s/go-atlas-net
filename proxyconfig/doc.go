// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

// Package proxyconfig validates and resolves shared outbound proxy settings.
//
// URL mode accepts HTTP, HTTPS, SOCKS5 and SOCKS5H endpoints with an explicit
// port and optional userinfo. Percent-encode @ and / in credentials. Only an
// empty or root path and empty query and fragment components are allowed.
// Host mode builds an HTTP URL from a resolver name or unbracketed IP address
// (including scoped IPv6), a port and optional [Auth].
//
// [Config.URLValue] returns credentials for transport use. Format the [Config]
// for diagnostics. Validation errors omit the supplied URL.
//
// # Usage
//
//	proxy := proxyconfig.Config{Mode: proxyconfig.ModeHost, Host: "proxy.example", Port: 3128}
//	u, err := proxy.URLValue()
package proxyconfig
