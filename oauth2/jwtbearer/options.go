// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package jwtbearer

//go:generate optgen generate

import (
	"time"

	"github.com/altessa-s/go-atlas-core/time/clock"
)

// DefaultExpiryMargin is the early refresh window, capped at half the token lifetime.
const DefaultExpiryMargin = 10 * time.Second

type options struct {
	clock          clock.Clock   `optgen:"default=clock.Real()"`
	expiryMargin   time.Duration `optgen:"default=DefaultExpiryMargin" optval:"positive=allow_zero"`
	scopes         []string
	subject        string
	audience       string
	keyID          string
	optionalExpiry bool
	insecureHTTP   bool
}
