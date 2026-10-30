// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package client

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"syscall"
	"testing"
)

func BenchmarkRedactTransportError(b *testing.B) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "dial refused", err: &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443},
			Err: syscall.ECONNREFUSED}},
		{name: "dns", err: &net.DNSError{Name: "example.org", IsNotFound: true}},
		{name: "unknown", err: fmt.Errorf("wrapped: %w", errors.New("custom"))},
	}
	for _, bc := range cases {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = redactTransportError(bc.err)
			}
		})
	}
}

func BenchmarkRedactedURL(b *testing.B) {
	u, err := url.Parse("https://user:pass@api.example.com/v1/orders/42?token=secret#frag")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = redactedURL(u)
	}
}
