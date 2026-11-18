// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

// Package jwtbearer signs and exchanges RFC 7523 assertions and caches access tokens.
//
// # Usage
//
//	source, err := jwtbearer.New(client, tokenURL, email, signer, jwtbearer.WithScopes(scope))
//	if err != nil { return err }
//	token, err := source.Token(ctx)
package jwtbearer
