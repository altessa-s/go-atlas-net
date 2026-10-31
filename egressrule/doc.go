// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

// Package egressrule is the grammar of outbound (egress) allow and deny
// rules, shared by the configuration schema (config/http.EgressRule) and the
// egress engine (net/http/client.EgressPolicy), so a rule is validated
// the same way wherever it is written.
//
// A [Rule] is the textual form a configuration file or a stored document
// holds: an optional name, a host glob, a CIDR and a port. [Rule.Parse]
// validates it and returns its [Matcher], the parsed and normalized form the
// engine compiles; [Check] validates a parsed matcher. A deny rule needs at
// least one matcher; an allow rule needs a CIDR, because allow rules only
// exempt resolved addresses from the default private denial.
//
// Host globs are [path.Match] patterns, case insensitive, "*" crossing dots.
// [NormalizeHostGlob] returns a glob in the form hosts are matched in, and
// [NormalizeHost] a requested host: lower case, the Unicode full stops IDNA
// reads as dots as ASCII dots, each internationalized label in its ASCII
// (punycode) form, the form net/http dials. [NormalizePrefix] turns an
// IPv4-mapped prefix of length 96 or more into the IPv4 prefix it covers,
// since a connection to an IPv4-mapped address is made over IPv4.
//
// # Usage
//
//	rule := egressrule.Rule{Host: "*.internal", Port: 22}
//	m, err := rule.Parse(egressrule.Deny, 0)
//	if err != nil {
//	    return err // "deny[0].host: ..."
//	}
//	_ = m.Host // "*.internal"
package egressrule
