// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package proxyconfig_test

import (
	"testing"

	"github.com/altessa-s/go-atlas-net/proxyconfig"
)

func BenchmarkURLValue(b *testing.B) {
	c := proxyconfig.Config{Mode: proxyconfig.ModeURL, URL: "socks5://proxy.example:1080"}
	for b.Loop() {
		if _, err := c.URLValue(); err != nil {
			b.Fatal(err)
		}
	}
}
