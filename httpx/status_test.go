// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package httpx_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/httpx"
)

func TestStatusClasses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		success  bool
		redirect bool
	}{
		{name: "zero", status: 0},
		{name: "negative", status: -200},
		{name: "continue", status: http.StatusContinue},
		{name: "199", status: 199},
		{name: "ok", status: http.StatusOK, success: true},
		{name: "no content", status: http.StatusNoContent, success: true},
		{name: "299", status: 299, success: true},
		{name: "multiple choices", status: http.StatusMultipleChoices, redirect: true},
		{name: "not modified", status: http.StatusNotModified, redirect: true},
		{name: "permanent redirect", status: http.StatusPermanentRedirect, redirect: true},
		{name: "399", status: 399, redirect: true},
		{name: "bad request", status: http.StatusBadRequest},
		{name: "internal error", status: http.StatusInternalServerError},
		{name: "1000", status: 1000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.success, httpx.IsSuccess(tc.status))
			require.Equal(t, tc.redirect, httpx.IsRedirect(tc.status))
		})
	}
}

func TestDoerFunc(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	var got *http.Request
	var d httpx.Doer = httpx.DoerFunc(func(req *http.Request) (*http.Response, error) {
		got = req
		return &http.Response{StatusCode: http.StatusTeapot}, errBoom
	})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com", http.NoBody)
	require.NoError(t, err)

	resp, err := d.Do(req) //nolint:bodyclose // synthetic response without a body
	require.ErrorIs(t, err, errBoom)
	require.Same(t, req, got)
	require.Equal(t, http.StatusTeapot, resp.StatusCode)
}

func TestHTTPClientIsDoer(t *testing.T) {
	t.Parallel()

	var _ httpx.Doer = &http.Client{}
}
