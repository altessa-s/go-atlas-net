// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package httpx

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHTTPHeaderGetter_GetHeader(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Add("X-Forwarded-For", "5.6.7.8")

	g := NewHTTPHeaderGetter(req)
	vals := g.GetHeader("X-Forwarded-For")
	require.Len(t, vals, 2)
}

func TestHTTPHeaderGetter_GetSingleHeader(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Content-Type", "application/json")

	g := NewHTTPHeaderGetter(req)
	got := g.GetSingleHeader("Content-Type")
	require.Equal(t, "application/json", got)
}

func TestHTTPHeaderGetter_NilRequest(t *testing.T) {
	g := NewHTTPHeaderGetter(nil)
	require.Nil(t, g)
}

func TestHTTPHeaderGetter_NilReceiver(t *testing.T) {
	var g *HTTPHeaderGetter
	vals := g.GetHeader("X-Test")
	require.Nil(t, vals)
	got := g.GetSingleHeader("X-Test")
	require.Equal(t, "", got)
}

func TestHTTPHeaderGetter_MissingHeader(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	g := NewHTTPHeaderGetter(req)

	vals := g.GetHeader("X-Missing")
	require.Equal(t, 0, len(vals))
	got := g.GetSingleHeader("X-Missing")
	require.Equal(t, "", got)
}
