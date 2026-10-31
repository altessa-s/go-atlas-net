// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package proxydial_test

import (
	"io"
	"net"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/proxydial"
)

// startAuthSOCKS5 serves a SOCKS5 server that accepts only username/password
// authentication (RFC 1929) with the given credentials. After a successful
// CONNECT it writes "hello" on the tunnel and closes it.
func startAuthSOCKS5(tb testing.TB, user, pass string) string {
	tb.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(tb, err)
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			wg.Go(func() { serveAuthSOCKS5(conn, user, pass) })
		}
	})
	tb.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	return ln.Addr().String()
}

func serveAuthSOCKS5(conn net.Conn, user, pass string) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}
	methods := make([]byte, head[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if !slices.Contains(methods, 2) {
		_, _ = conn.Write([]byte{5, 0xff}) // no acceptable method
		return
	}
	if _, err := conn.Write([]byte{5, 2}); err != nil {
		return
	}

	ver := make([]byte, 2)
	if _, err := io.ReadFull(conn, ver); err != nil {
		return
	}
	gotUser := make([]byte, ver[1])
	if _, err := io.ReadFull(conn, gotUser); err != nil {
		return
	}
	plen := make([]byte, 1)
	if _, err := io.ReadFull(conn, plen); err != nil {
		return
	}
	gotPass := make([]byte, plen[0])
	if _, err := io.ReadFull(conn, gotPass); err != nil {
		return
	}
	if string(gotUser) != user || string(gotPass) != pass {
		_, _ = conn.Write([]byte{1, 1})
		return
	}
	if _, err := conn.Write([]byte{1, 0}); err != nil {
		return
	}

	// CONNECT with a domain name destination: VER CMD RSV ATYP LEN NAME PORT.
	req := make([]byte, 5)
	if _, err := io.ReadFull(conn, req); err != nil {
		return
	}
	if _, err := io.ReadFull(conn, make([]byte, int(req[4])+2)); err != nil {
		return
	}
	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	_, _ = io.WriteString(conn, "hello")
}

func TestSOCKS5Dialer_UsernamePassword(t *testing.T) {
	t.Parallel()

	addr := startAuthSOCKS5(t, "svc", "s3cret")
	cases := []struct {
		name    string
		user    *url.Userinfo
		wantErr bool
	}{
		{name: "valid_credentials", user: url.UserPassword("svc", "s3cret")},
		{name: "wrong_password", user: url.UserPassword("svc", "nope"), wantErr: true},
		{name: "no_credentials", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dial, err := proxydial.SOCKS5Dialer(&url.URL{Scheme: "socks5", Host: addr, User: tc.user}, &net.Dialer{})
			require.NoError(t, err)
			conn, err := dial(t.Context(), "tcp6", "target.example:443")
			if tc.wantErr {
				require.Error(t, err)
				require.Nil(t, conn)
				return
			}
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
			require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
			got, err := io.ReadAll(conn)
			require.NoError(t, err)
			require.Equal(t, "hello", string(got))
		})
	}
}
