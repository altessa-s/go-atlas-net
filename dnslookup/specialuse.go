// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package dnslookup

import "strings"

// specialUseDomains is the IANA Special-Use Domain Names registry snapshot
// retrieved 2026-11-28 (registry last updated 2026-05-22).
// https://www.iana.org/assignments/special-use-domain-names
var specialUseDomains = map[string]struct{}{
	"alt":                      {},
	"6tisch.arpa":              {},
	"eap.arpa":                 {},
	"eap-noob.arpa":            {}, // Deprecated, still listed in the registry.
	"home.arpa":                {},
	"10.in-addr.arpa":          {},
	"254.169.in-addr.arpa":     {},
	"16.172.in-addr.arpa":      {},
	"17.172.in-addr.arpa":      {},
	"18.172.in-addr.arpa":      {},
	"19.172.in-addr.arpa":      {},
	"20.172.in-addr.arpa":      {},
	"21.172.in-addr.arpa":      {},
	"22.172.in-addr.arpa":      {},
	"23.172.in-addr.arpa":      {},
	"24.172.in-addr.arpa":      {},
	"25.172.in-addr.arpa":      {},
	"26.172.in-addr.arpa":      {},
	"27.172.in-addr.arpa":      {},
	"28.172.in-addr.arpa":      {},
	"29.172.in-addr.arpa":      {},
	"30.172.in-addr.arpa":      {},
	"31.172.in-addr.arpa":      {},
	"170.0.0.192.in-addr.arpa": {},
	"171.0.0.192.in-addr.arpa": {},
	"168.192.in-addr.arpa":     {},
	"8.e.f.ip6.arpa":           {},
	"9.e.f.ip6.arpa":           {},
	"a.e.f.ip6.arpa":           {},
	"b.e.f.ip6.arpa":           {},
	"ipv4only.arpa":            {},
	"resolver.arpa":            {},
	"service.arpa":             {},
	"example":                  {},
	"example.com":              {},
	"example.net":              {},
	"example.org":              {},
	"invalid":                  {},
	"local":                    {},
	"localhost":                {},
	"onion":                    {},
	"test":                     {},
}

func isSpecialUse(name string) bool {
	for name != "" {
		if _, found := specialUseDomains[name]; found {
			return true
		}

		_, name, _ = strings.Cut(name, ".")
	}

	return false
}
