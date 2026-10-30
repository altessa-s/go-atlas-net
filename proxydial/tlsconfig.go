// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package proxydial

import (
	"crypto/tls"
	"net/url"
)

// TLSConfig returns the *tls.Config to use for the TLS handshake to
// an https:// proxy. A nil user produces the safe default
// (ServerName=proxyURL.Hostname(), MinVersion=TLS12). A non-nil user
// is cloned, then ServerName and MinVersion are filled in only if
// the caller left them at their zero values — every other field
// (RootCAs, Certificates, InsecureSkipVerify, …) is preserved
// verbatim. The caller's *tls.Config is never mutated.
func TLSConfig(proxyURL *url.URL, user *tls.Config) *tls.Config {
	if user == nil {
		return &tls.Config{
			ServerName: proxyURL.Hostname(),
			MinVersion: tls.VersionTLS12,
		}
	}
	cfg := user.Clone()
	if cfg.ServerName == "" {
		cfg.ServerName = proxyURL.Hostname()
	}
	if cfg.MinVersion == 0 {
		cfg.MinVersion = tls.VersionTLS12
	}
	return cfg
}
