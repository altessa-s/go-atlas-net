// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package proxydial

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The proxy sends rejection headers but leaves its advertised body pending.
type rejectedConn struct {
	net.Conn
	header    *strings.Reader
	bodyReads int
}

func (c *rejectedConn) Read(p []byte) (int, error) {
	if c.header.Len() > 0 {
		return c.header.Read(p)
	}
	c.bodyReads++
	return 0, io.EOF
}
func (*rejectedConn) Write(p []byte) (int, error) { return len(p), nil }
func (*rejectedConn) SetDeadline(time.Time) error { return nil }

func TestConnectRejectionDoesNotDrainBody(t *testing.T) {
	t.Parallel()
	conn := &rejectedConn{header: strings.NewReader("HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 1000000\r\n\r\n")}
	tunnel, err := sendConnect(t.Context(), conn, "target:443", nil)
	require.Error(t, err)
	require.Nil(t, tunnel)
	require.Zero(t, conn.bodyReads, "rejection must return without waiting for the body")
}
