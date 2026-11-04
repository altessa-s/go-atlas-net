// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package hmacsign_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/altessa-s/go-atlas-net/hmacsign"
)

func BenchmarkSign(b *testing.B) {
	signer := mustSigner(b, hmacsign.Stripe(), []byte("whsec_bench_secret_01"))
	body := []byte(`{"event":"payment.succeeded","id":"evt_bench"}`)

	b.ReportAllocs()

	for b.Loop() {
		_ = signer.Sign(body)
	}
}

func BenchmarkVerify(b *testing.B) {
	secret := []byte("whsec_bench_secret_01")
	body := []byte(`{"event":"payment.succeeded","id":"evt_bench"}`)
	signer := mustSigner(b, hmacsign.Stripe(), secret)
	verifier := mustVerifier(b, hmacsign.Stripe(), secret, hmacsign.WithTolerance(0))
	header := signer.Sign(body)

	b.ReportAllocs()

	for b.Loop() {
		_ = verifier.Verify(header, body)
	}
}

func BenchmarkVerifyWithReplayCache(b *testing.B) {
	secret := []byte("whsec_bench_secret_01")
	body := []byte(`{"event":"payment.succeeded","id":"evt_bench"}`)
	signer := mustSigner(b, hmacsign.Stripe(), secret)
	verifier := mustVerifier(b, hmacsign.Stripe(), secret, hmacsign.WithReplayCache(hmacsign.NewMemoryReplayCache()))
	// Distinct bodies keep every signature fresh so each iteration takes the
	// accept-and-record path rather than the replay rejection.
	headers := make([]string, 1024)

	bodies := make([][]byte, len(headers))
	for i := range headers {
		bodies[i] = []byte(string(body) + strconv.Itoa(i))
		headers[i] = signer.Sign(bodies[i])
	}

	b.ReportAllocs()

	i := 0
	for b.Loop() {
		_ = verifier.Verify(headers[i%len(headers)], bodies[i%len(headers)])
		i++
	}
}

func BenchmarkMemoryReplayCache(b *testing.B) {
	c := hmacsign.NewMemoryReplayCache()

	keys := make([]string, 4096)
	for i := range keys {
		keys[i] = strconv.Itoa(i)
	}

	b.ReportAllocs()

	i := 0
	for b.Loop() {
		_, _ = c.CheckAndStore(keys[i%len(keys)], time.Minute)
		i++
	}
}
