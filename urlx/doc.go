// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

// Package urlx validates, joins and canonicalizes the base URLs of services a
// client talks to.
//
// A configured base URL is usually concatenated with an API path, and often
// with a credential in that path. A URL that ends in a bare "?" or "#"
// silently moves everything appended after it into the query or the
// fragment, and userinfo leaks a credential into logs. ParseBaseURL rejects
// those forms in one place instead of every caller re-implementing a subset
// of the checks.
//
// # Parsing
//
// ParseBaseURL accepts an absolute URL with an allowed scheme (http and https
// by default) and a host, and rejects userinfo, a query (including a bare
// "?"), and a fragment (including a bare "#"). In base mode (the default) the
// trailing slashes of the path are trimmed; WithEndpointMode keeps a full
// endpoint path unchanged. Every failure matches ErrInvalidURL, which wraps
// core/errors.ErrInvalidArgument, and the error text never contains the URL.
// A URL valid except for its scheme also matches ErrSchemeNotAllowed.
//
// # Joining
//
// JoinPath appends literal path elements to a parsed base, escaping each
// segment, so an element never starts a query or fragment and the escaped
// base path ("%2F" included) is kept byte for byte.
//
// # Canonical form
//
// Canonical renders a URL for comparison: lower-case scheme and host, the
// escaped path without trailing slashes, and no userinfo, query or fragment.
//
// # Usage
//
//	import "github.com/altessa-s/go-atlas-net/urlx"
//
//	base, err := urlx.ParseBaseURL(cfg.BaseURL)
//	if err != nil {
//	    return fmt.Errorf("%w: base URL: %w", ErrConfig, err)
//	}
//	endpoint := urlx.JoinPath(base, "bot"+token, "sendMessage")
//	scope := urlx.Canonical(base) // key a stored credential by endpoint
package urlx
