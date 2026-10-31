// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package hostpacer_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/altessa-s/go-atlas-net/http/client/limiters/hostpacer"
)

func BenchmarkPacer_Allow(b *testing.B) {
	p, err := hostpacer.New(time.Nanosecond, 1<<30)
	if err != nil {
		b.Fatal(err)
	}
	ctx := b.Context()
	for b.Loop() {
		_ = p.Allow(ctx, "api.example.com")
	}
}

// BenchmarkPacer_AllowManyHosts spreads requests over many hosts, so bucket
// lookup and creation dominate, with and without the soft host bound.
func BenchmarkPacer_AllowManyHosts(b *testing.B) {
	hosts := make([]string, 4096)
	for i := range hosts {
		hosts[i] = "h" + strconv.Itoa(i) + ".example.com"
	}
	cases := []struct {
		name string
		opts []hostpacer.Option
	}{
		{"unbounded", nil},
		{"max_hosts_1024", []hostpacer.Option{hostpacer.WithMaxHosts(1024)}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			p, err := hostpacer.New(time.Nanosecond, 1<<30, tc.opts...)
			if err != nil {
				b.Fatal(err)
			}
			ctx := b.Context()
			i := 0
			b.ReportAllocs()
			for b.Loop() {
				_ = p.Allow(ctx, hosts[i%len(hosts)])
				i++
			}
		})
	}
}
