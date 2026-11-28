// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

// Package dnslookup queries DNS MX records with egress name checks.
//
// # Query names
//
// LookupMX normalizes the host and passes it to the resolver as an absolute DNS
// name. Invalid names, IANA special-use names, and names denied by
// [WithEgressPolicy] match [ErrRefused] before any query.
//
// # Results
//
// [Lookup.LookupMX] preserves resolver order, DNS preferences, and the root
// exchange ".", including in partial answers. Callers interpret, sort, and
// limit results as needed.
//
// # Usage
//
//	lookup := dnslookup.New(dnslookup.WithMaxTimeout(3 * time.Second))
//	records, err := lookup.LookupMX(ctx, "contoso.com")
package dnslookup
