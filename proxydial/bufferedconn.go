// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package proxydial

import (
	"bufio"
	"net"
	"sync"
)

// newBufferedConn returns conn unchanged when br holds no bytes, and
// otherwise a wrapper that serves the buffered bytes first. The wrapper
// keeps CloseWrite (and CloseRead) when conn has them, so half-close
// works on the tunnel as on the bare connection.
func newBufferedConn(conn net.Conn, br *bufio.Reader) net.Conn {
	n := br.Buffered()
	if n == 0 {
		return conn
	}
	peeked, _ := br.Peek(n) // n bytes are buffered: Peek cannot fail
	bc := &bufferedConn{Conn: conn, pending: append([]byte(nil), peeked...)}
	cw, ok := conn.(closeWriter)
	if !ok {
		return bc
	}
	if cr, ok := conn.(closeReader); ok {
		return &bufferedHalfCloseConn{bufferedConn: bc, cw: cw, cr: cr}
	}
	return &bufferedCloseWriteConn{bufferedConn: bc, cw: cw}
}

type closeWriter interface{ CloseWrite() error }

type closeReader interface{ CloseRead() error }

// bufferedConn is a net.Conn whose first reads drain bytes buffered
// while parsing the CONNECT response; every other method delegates.
type bufferedConn struct {
	net.Conn

	mu      sync.Mutex
	pending []byte
}

// Read returns buffered bytes first, then reads from the connection.
func (c *bufferedConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if len(c.pending) > 0 {
		n := copy(p, c.pending)
		c.pending = c.pending[n:]
		if len(c.pending) == 0 {
			c.pending = nil
		}
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()
	return c.Conn.Read(p)
}

// bufferedCloseWriteConn is a bufferedConn over a connection that
// supports CloseWrite (e.g. *tls.Conn).
type bufferedCloseWriteConn struct {
	*bufferedConn

	cw closeWriter
}

// CloseWrite shuts down the writing side of the connection.
func (c *bufferedCloseWriteConn) CloseWrite() error { return c.cw.CloseWrite() }

// bufferedHalfCloseConn is a bufferedConn over a connection that
// supports both CloseWrite and CloseRead (e.g. *net.TCPConn).
type bufferedHalfCloseConn struct {
	*bufferedConn

	cw closeWriter
	cr closeReader
}

// CloseWrite shuts down the writing side of the connection.
func (c *bufferedHalfCloseConn) CloseWrite() error { return c.cw.CloseWrite() }

// CloseRead shuts down the reading side of the connection.
func (c *bufferedHalfCloseConn) CloseRead() error { return c.cr.CloseRead() }
