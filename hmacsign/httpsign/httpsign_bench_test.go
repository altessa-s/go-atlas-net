// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package httpsign_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/altessa-s/go-atlas-net/hmacsign"
	"github.com/altessa-s/go-atlas-net/hmacsign/httpsign"
)

func BenchmarkMiddleware(b *testing.B) {
	v := mustVerifier(b)
	body := `{"event":"payment.succeeded","id":"evt_bench"}`

	signer, err := hmacsign.NewSigner(hmacsign.GitHub(), []byte(secret))
	if err != nil {
		b.Fatal(err)
	}

	header := signer.Sign([]byte(body))
	handler := httpsign.Middleware(v)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
	}))

	b.ReportAllocs()

	for b.Loop() {
		req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
		req.Header.Set(v.HeaderName(), header)
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}
}
