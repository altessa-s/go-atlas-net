// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package httpx_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/httpx"
)

func TestParseBearer(t *testing.T) {
	t.Parallel()

	limit := strings.Repeat("a", httpx.MaxBearerLength)
	for _, tc := range []struct {
		name   string
		values []string
		token  string
		err    error
	}{
		{"missing", nil, "", httpx.ErrBearerMissing},
		{"multiple", []string{"Bearer abc", "Bearer abc"}, "", httpx.ErrBearerMultiple},
		{"empty header", []string{""}, "", httpx.ErrBearerMalformed},
		{"simple", []string{"Bearer abc"}, "abc", nil},
		{"mixed case", []string{"bEaReR a+/="}, "a+/=", nil},
		{"alphabet and padding", []string{"BEARER aZ9-._~+/==="}, "aZ9-._~+/===", nil},
		{"missing separator", []string{"Bearer"}, "", httpx.ErrBearerMalformed},
		{"wrong scheme", []string{"Basic abc"}, "", httpx.ErrBearerMalformed},
		{"embedded space", []string{"Bearer a b"}, "", httpx.ErrBearerMalformed},
		{"newline", []string{"Bearer a\nb"}, "", httpx.ErrBearerMalformed},
		{"embedded padding", []string{"Bearer a=b"}, "", httpx.ErrBearerMalformed},
		{"empty token", []string{"Bearer "}, "", httpx.ErrBearerMalformed},
		{"non-ASCII", []string{"Bearer é"}, "", httpx.ErrBearerMalformed},
		{"padding only", []string{"Bearer ="}, "", httpx.ErrBearerMalformed},
		{"double padding only", []string{"Bearer =="}, "", httpx.ErrBearerMalformed},
		{"two spaces", []string{"Bearer  abc"}, "", httpx.ErrBearerMalformed},
		{"tab", []string{"Bearer\tabc"}, "", httpx.ErrBearerMalformed},
		{"joined headers", []string{"Bearer a, Bearer b"}, "", httpx.ErrBearerMalformed},
		{"at limit", []string{"Bearer " + limit}, limit, nil},
		{"over limit including padding", []string{"Bearer " + limit + "="}, "", httpx.ErrBearerTooLong},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			token, err := httpx.ParseBearer(tc.values)
			require.ErrorIs(t, err, tc.err)
			require.Equal(t, tc.token, token)
		})
	}
}

func FuzzParseBearer(f *testing.F) {
	for _, header := range []string{"Bearer abc", "", "Bearer a=b", "Bearer a\nb", "bEaReR +/=="} {
		f.Add(header)
	}

	grammar := regexp.MustCompile(`^(?i:Bearer) ([A-Za-z0-9._~+/-]+=*)$`)

	f.Fuzz(func(t *testing.T, header string) {
		token, err := httpx.ParseBearer([]string{header})

		match := grammar.FindStringSubmatch(header)
		if match == nil || len(match[1]) > httpx.MaxBearerLength {
			require.Error(t, err)
			require.Empty(t, token)

			return
		}

		require.NoError(t, err)
		require.Equal(t, match[1], token)
	})
}
