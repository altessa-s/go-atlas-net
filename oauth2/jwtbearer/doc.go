// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

// Package jwtbearer signs and exchanges RFC 7523 assertions and caches access tokens.
//
// The endpoint query is preserved in the POST and the default audience.
// WithMaxResponseBytes bounds the reply and WithMaxLifetime caps the issued
// token lifetime. Unknown token response fields are ignored.
//
// Transport errors match ErrRequest and retain the original cause for errors.Is
// and errors.As. ResponseError exposes HTTP status and bounded rejection details.
// The returned error text omits credentials and provider diagnostics.
//
// # Usage
//
//	source, err := jwtbearer.New(client, tokenURL, email, signer, jwtbearer.WithScopes(scope))
//	if err != nil { return err }
//	token, err := source.Token(ctx)
package jwtbearer
