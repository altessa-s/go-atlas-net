// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

// Package dnslookup queries DNS MX records with egress name checks and bounded results.
//
// # Query names
//
// LookupMX normalizes the host and passes it to the resolver as an absolute DNS
// name. Invalid and IANA special-use names are refused before any query.
// Use [WithEgressPolicy] to also deny names in operator-defined zones.
//
// # Usage
//
//	lookup := dnslookup.New(dnslookup.WithMaxTimeout(3 * time.Second))
//	records, err := lookup.LookupMX(ctx, "contoso.com")
//
// See [Lookup.LookupMX] for result ordering, null MX, and partial answers.
package dnslookup
