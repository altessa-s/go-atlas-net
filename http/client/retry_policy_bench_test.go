// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client_test

import (
	"errors"
	"net/http"
	"testing"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

func BenchmarkDefaultRetryPolicy(b *testing.B) {
	req, err := http.NewRequestWithContext(b.Context(), http.MethodGet, "http://api.test/v1/items?token=x", nil)
	if err != nil {
		b.Fatal(err)
	}
	cases := []struct {
		name string
		resp *http.Response
		err  error
	}{
		{name: "ok", resp: &http.Response{StatusCode: http.StatusOK, Request: req}},
		{name: "retryable status", resp: &http.Response{StatusCode: http.StatusServiceUnavailable, Request: req}},
		{name: "failed status", resp: &http.Response{StatusCode: http.StatusNotFound, Request: req}},
		{name: "transport error", err: errors.New("connection reset")},
	}
	for _, bc := range cases {
		b.Run(bc.name, func(b *testing.B) {
			ctx := b.Context()
			b.ReportAllocs()
			for b.Loop() {
				_, _ = httpclient.DefaultRetryPolicy(ctx, bc.resp, bc.err)
			}
		})
	}
}
