// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

// Package proxyconfig validates and resolves shared outbound proxy settings.
//
// # Usage
//
//	proxy := proxyconfig.Config{Mode: proxyconfig.ModeHost, Host: "proxy.example", Port: 3128}
//	u, err := proxy.URLValue()
package proxyconfig
