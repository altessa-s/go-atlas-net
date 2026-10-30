// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package proxydial

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// MaxConnectResponseHeaderBytes bounds the status line and headers of a
// proxy's CONNECT response.
const MaxConnectResponseHeaderBytes = 64 << 10

// ErrConnectResponseTooLarge is returned when a proxy's CONNECT response
// header exceeds [MaxConnectResponseHeaderBytes].
var ErrConnectResponseTooLarge = errors.New("proxydial: CONNECT response header too large")

// HTTPConnect opens TCP+TLS to proxyURL and sends an HTTP CONNECT
// request for addr (RFC 7231 §4.3.6), returning the tunneled
// net.Conn. tlsCfg is consulted only when proxyURL.Scheme is "https"
// — for plain "http" it is ignored. The supplied dialer controls
// TCP-level options (timeout, keep-alive); pass &net.Dialer{} for
// stdlib defaults.
//
// Caller's tlsCfg is cloned via [TLSConfig] before use; user input
// is never mutated.
//
// Bytes the target sent right behind the 200 response (the greeting of
// a server-first protocol such as SMTP, FTP, POP3 or SSH) are kept and
// returned by the first reads on the tunnel.
func HTTPConnect(ctx context.Context, dialer *net.Dialer, proxyURL *url.URL, tlsCfg *tls.Config, addr string) (net.Conn, error) {
	conn, err := dialer.DialContext(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return nil, fmt.Errorf("dial proxy %s: %w", proxyURL.Host, err)
	}
	if proxyURL.Scheme == "https" {
		tlsConn := tls.Client(conn, TLSConfig(proxyURL, tlsCfg))
		if err = tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("tls handshake to proxy %s: %w", proxyURL.Host, err)
		}
		conn = tlsConn
	}
	tunnel, err := sendConnect(ctx, conn, addr, proxyURL.User)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tunnel, nil
}

// sendConnect writes a CONNECT request to conn and validates the
// response, returning a non-2xx status as an error. A 200 response has
// no content (RFC 9110 §9.3.6): its Content-Length and
// Transfer-Encoding are ignored and every byte after the header block
// belongs to the tunnel. When bufio already holds such bytes, the
// returned conn serves them before reading from conn; otherwise conn
// itself is returned. Canceling ctx interrupts the handshake I/O, and
// the returned error then wraps ctx.Err().
func sendConnect(ctx context.Context, conn net.Conn, addr string, auth *url.Userinfo) (_ net.Conn, err error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	// A deadline in the past unblocks the pending Write/Read on cancellation.
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Unix(1, 0))
		close(interrupted)
	})
	defer func() {
		if !stop() {
			// Wait for the callback so it cannot poison the deadline after the
			// reset below; the conn is unusable either way, so report ctx.
			<-interrupted
			err = fmt.Errorf("CONNECT to %s: %w", addr, ctx.Err())
		}
		_ = conn.SetDeadline(time.Time{})
	}()

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if auth != nil {
		req.Header.Set("Proxy-Authorization", basicAuthHeader(auth))
	}

	if err = req.Write(conn); err != nil {
		return nil, fmt.Errorf("write CONNECT to %s: %w", addr, err)
	}

	// Bound the status line and headers, as net/http bounds a CONNECT
	// response: a proxy must not exhaust memory before the tunnel exists. The
	// limit wraps only the parser's reads; the tunnel then reads conn.
	limited := &io.LimitedReader{R: conn, N: MaxConnectResponseHeaderBytes}
	br := bufio.NewReader(limited)
	resp, err := http.ReadResponse(br, req) //nolint:bodyclose // HTTPConnect owns conn; closing this body would drain tunnel or rejection bytes.
	if err != nil {
		if limited.N <= 0 {
			return nil, fmt.Errorf("read CONNECT response from %s: %w", addr, ErrConnectResponseTooLarge)
		}
		return nil, fmt.Errorf("read CONNECT response from %s: %w", addr, err)
	}
	if resp.StatusCode != http.StatusOK {
		// HTTPConnect closes the failed connection. Closing this response's
		// body would drain it first and could block on an untrusted proxy.
		return nil, fmt.Errorf("proxy CONNECT to %s: %s", addr, resp.Status)
	}
	// The body of a 200 is never read or closed: closing it would consume
	// tunnel bytes as Content-Length or chunked content.
	return newBufferedConn(conn, br), nil
}

// basicAuthHeader formats *url.Userinfo as a "Basic <base64>" value
// suitable for the Proxy-Authorization header.
func basicAuthHeader(u *url.Userinfo) string {
	user := u.Username()
	pwd, _ := u.Password()
	creds := base64.StdEncoding.EncodeToString([]byte(user + ":" + pwd))
	return "Basic " + creds
}
