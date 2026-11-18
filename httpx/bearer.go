// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package httpx

import (
	"errors"
	"strings"
)

// MaxBearerLength is the maximum token length in bytes, including padding.
const MaxBearerLength = 4096

var (
	// ErrBearerMissing indicates that no Authorization value was supplied.
	ErrBearerMissing = errors.New("missing authorization header")
	// ErrBearerMultiple indicates that more than one Authorization value was supplied.
	ErrBearerMultiple = errors.New("multiple authorization headers")
	// ErrBearerMalformed indicates an invalid bearer scheme, separator, or token.
	ErrBearerMalformed = errors.New("malformed bearer credential")
	// ErrBearerTooLong indicates a bearer token exceeding MaxBearerLength.
	ErrBearerTooLong = errors.New("bearer token too long")
)

// ParseBearer extracts an RFC 6750 token from exactly one Authorization value.
// It requires a case-insensitive Bearer scheme, one space, and a token68
// credential of at most [MaxBearerLength] bytes. Pass all header values so
// duplicate credentials can be rejected. On error it returns an empty token.
func ParseBearer(values []string) (string, error) {
	if len(values) == 0 {
		return "", ErrBearerMissing
	}

	if len(values) > 1 {
		return "", ErrBearerMultiple
	}

	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", ErrBearerMalformed
	}

	if len(token) > MaxBearerLength {
		return "", ErrBearerTooLong
	}

	data := strings.TrimRight(token, "=")
	if data == "" {
		return "", ErrBearerMalformed
	}

	for _, ch := range data {
		valid := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("-._~+/", ch)
		if !valid {
			return "", ErrBearerMalformed
		}
	}

	return token, nil
}
