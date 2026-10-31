// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package proxydial

import (
	"context"
	"net"
	"time"
)

// DialContextFunc is the standard net.Dialer signature.
type DialContextFunc = func(ctx context.Context, network, addr string) (net.Conn, error)

// Default TCP-level options for the dialer used to reach the proxy.
// These mirror the values the HTTP client uses for its pooled
// transport, so consumers wiring [HTTPConnect] / [SOCKS5Dialer] get
// the same timeout/keep-alive defaults whether they go through the
// HTTP or gRPC client.
const (
	DefaultDialTimeout   = 30 * time.Second
	DefaultDialKeepAlive = 30 * time.Second
)

// DefaultDialer returns a *net.Dialer pre-configured with
// [DefaultDialTimeout] and [DefaultDialKeepAlive]. Use it as the
// dialer argument to [HTTPConnect] and [SOCKS5Dialer] unless callers
// have a specific reason to override the timeouts.
func DefaultDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   DefaultDialTimeout,
		KeepAlive: DefaultDialKeepAlive,
	}
}
