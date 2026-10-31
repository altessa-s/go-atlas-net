// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package egressrule_test

import (
	"testing"

	"github.com/altessa-s/go-atlas-net/egressrule"
)

func BenchmarkRuleParse(b *testing.B) {
	rule := egressrule.Rule{Name: "blockSSH", Host: "*.Internal.example", CIDR: "10.0.0.0/8", Port: 22}
	for b.Loop() {
		if _, err := rule.Parse(egressrule.Deny, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNormalizeHost(b *testing.B) {
	for b.Loop() {
		_ = egressrule.NormalizeHost("bücher.Example.")
	}
}
