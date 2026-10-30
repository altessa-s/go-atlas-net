// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package egressrule

import (
	"errors"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// List is the policy list a rule belongs to; it decides which matchers the
// rule needs and names the rule in errors ("deny[1].host").
type List string

// Policy lists.
const (
	// Allow rules exempt resolved addresses from the default private denial,
	// so each one needs a CIDR; host and port narrow it.
	Allow List = "allow"
	// Deny rules refuse a destination; each one needs at least one matcher.
	Deny List = "deny"
)

// MaxNameLen bounds the length of a rule name.
const MaxNameLen = 64

// MaxPort is the largest destination port.
const MaxPort = 65535

// ipv4MappedBits is the length of the ::ffff:0:0/96 IPv4-mapped prefix.
const ipv4MappedBits = 96

// Rule is an egress rule in its textual form, as a configuration file or a
// stored document holds it. Every matcher that is set must match (AND); a
// zero matcher matches anything. [Rule.Parse] validates it and returns its
// [Matcher].
type Rule struct {
	// Name identifies the rule in decisions and refusals: up to 64
	// characters of A-Z, a-z, 0-9, '_', '.', '-', '[' and ']'. Empty names
	// it by its list and position ("deny[0]"). It is not a matcher.
	Name string `yaml:"name" json:"name,omitempty"`
	// Host is a case-insensitive [path.Match] glob of the requested host
	// name; "*" also matches dots. See [NormalizeHostGlob].
	Host string `yaml:"host" json:"host,omitempty"`
	// CIDR matches the resolved IP address: an IPv4 or IPv6 prefix, or an
	// IPv4-mapped one of length 96 or more (the IPv4 prefix it covers).
	CIDR string `yaml:"cidr" json:"cidr,omitempty"`
	// Port matches the destination port: 0 (any) or 1 to 65535.
	Port int `yaml:"port" json:"port,omitempty"`
}

// Matcher is a rule in parsed form. [Check] validates it and returns it
// normalized: the host glob in its ASCII form, the prefix masked and an
// IPv4-mapped prefix as the IPv4 prefix it covers.
type Matcher struct {
	// Name is the rule name, empty for none.
	Name string
	// Host is the host glob, empty for any host.
	Host string
	// CIDR is the address prefix; the zero prefix matches any address.
	CIDR netip.Prefix
	// Port is the destination port, 0 for any port.
	Port uint16
}

// Parse validates r as rule index of list and returns its normalized
// [Matcher]. It rejects a CIDR that does not parse, a port outside 0 to
// 65535 and everything [Check] rejects. Errors name the rule
// ("allow[2].cidr").
func (r Rule) Parse(list List, index int) (Matcher, error) {
	name := label(list, index)
	m := Matcher{Name: r.Name, Host: r.Host}
	if r.CIDR != "" {
		prefix, err := netip.ParsePrefix(r.CIDR)
		if err != nil {
			return Matcher{}, fmt.Errorf("%s.cidr: %w", name, err)
		}
		m.CIDR = prefix
	}
	if r.Port < 0 || r.Port > MaxPort {
		return Matcher{}, fmt.Errorf("%s.port: must be 0 (any) or 1 to %d", name, MaxPort)
	}
	m.Port = uint16(r.Port)
	return Check(list, index, m)
}

// Validate parses every allow and deny rule ([Rule.Parse]) and returns the
// first error, deny rules first.
func Validate(allow, deny []Rule) error {
	for i, r := range deny {
		if _, err := r.Parse(Deny, i); err != nil {
			return err
		}
	}
	for i, r := range allow {
		if _, err := r.Parse(Allow, i); err != nil {
			return err
		}
	}
	return nil
}

// Check validates m as rule index of list and returns it normalized. A deny
// rule needs at least one matcher (a name is not one); an allow rule needs a
// CIDR, because allow rules only exempt resolved addresses from the default
// private denial; a name has the [Rule.Name] shape; a host glob passes
// [NormalizeHostGlob]; a non-zero prefix is valid, and an IPv4-mapped one is
// at least 96 bits long. Errors name the rule ("deny[1].host").
func Check(list List, index int, m Matcher) (Matcher, error) {
	name := label(list, index)
	switch list {
	case Deny:
		if m.Host == "" && m.CIDR == (netip.Prefix{}) && m.Port == 0 {
			return Matcher{}, fmt.Errorf("%s: at least one matcher is required", name)
		}
	case Allow:
		if m.CIDR == (netip.Prefix{}) {
			return Matcher{}, fmt.Errorf("%s.cidr: required, allow rules only exempt resolved addresses", name)
		}
	default:
		return Matcher{}, fmt.Errorf("%s: unknown list", name)
	}
	if m.Name != "" && !nameRe.MatchString(m.Name) {
		return Matcher{}, fmt.Errorf("%s.name: expected 1 to %d characters of A-Z, a-z, 0-9, '_', '.', '-', '[' and ']'",
			name, MaxNameLen)
	}
	if m.Host != "" {
		host, err := NormalizeHostGlob(m.Host)
		if err != nil {
			return Matcher{}, fmt.Errorf("%s.host: %w", name, err)
		}
		m.Host = host
	}
	if m.CIDR != (netip.Prefix{}) {
		if !m.CIDR.IsValid() {
			return Matcher{}, fmt.Errorf("%s.cidr: invalid prefix", name)
		}
		if m.CIDR.Addr().Is4In6() && m.CIDR.Bits() < ipv4MappedBits {
			return Matcher{}, fmt.Errorf("%s.cidr: an IPv4-mapped prefix needs a length of %d or more", name, ipv4MappedBits)
		}
		m.CIDR = NormalizePrefix(m.CIDR)
	}
	return m, nil
}

// label names rule index of list ("deny[0]").
func label(list List, index int) string {
	return string(list) + "[" + strconv.Itoa(index) + "]"
}

// NormalizePrefix masks p and turns an IPv4-mapped prefix of length 96 or
// more into the IPv4 prefix it covers: a connection to an IPv4-mapped
// address is made over IPv4, so it is matched as the IPv4 address.
func NormalizePrefix(p netip.Prefix) netip.Prefix {
	p = p.Masked()
	if p.Addr().Is4In6() && p.Bits() >= ipv4MappedBits {
		return netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-ipv4MappedBits)
	}
	return p
}

// nameRe is the shape of a rule name: a short identifier ("blockMetadata",
// "deny[3]", "allow.private") that is safe to render in error messages.
var nameRe = regexp.MustCompile(`\A[A-Za-z0-9_.\-\[\]]{1,64}\z`)

// unicodeDots maps the label separators IDNA (UTS #46) reads as a full stop — ideographic, fullwidth and halfwidth ideographic
// full stops — to ".", so a rule or host spelled with them splits into the labels net/http dials.
var unicodeDots = strings.NewReplacer("。", ".", "．", ".", "｡", ".")

// errBadHostGlob is the shape error of a host glob.
var errBadHostGlob = errors.New("expected a DNS glob without scheme, port, path or trailing dot")

// NormalizeHostGlob validates a host glob and returns it in the form hosts
// are matched in ([NormalizeHost]): lower case, labels separated by ASCII
// dots (the Unicode full stops U+3002, U+FF0E and U+FF61 read as dots), each
// Unicode label in its ASCII (punycode) form under the IDNA lookup profile.
// A glob is rejected when it is empty, holds a scheme, port, path, user
// info, space or tab, ends in a dot, is not a valid [path.Match] pattern, or
// has a Unicode label that holds a glob metacharacter, is not a valid
// internationalized name, holds a label separator or maps to nothing (a lone
// soft hyphen, U+00AD, would turn "example.<U+00AD>" into the trailing dot
// form "example."); the normalized form is checked again, so no glob
// normalizes into a malformed one.
func NormalizeHostGlob(pattern string) (string, error) {
	if err := checkHostGlob(pattern); err != nil {
		return "", err
	}
	host, err := asciiHostGlob(pattern)
	if err != nil {
		return "", err
	}
	if host != pattern {
		if err := checkHostGlob(host); err != nil {
			return "", fmt.Errorf("normalized to %q: %w", host, err)
		}
	}
	return host, nil
}

// checkHostGlob checks the shape of a host glob in one spelling.
func checkHostGlob(pattern string) error {
	if pattern == "" {
		return errors.New("empty host glob")
	}
	if host := unicodeDots.Replace(pattern); strings.ContainsAny(host, ":/@ \t") || strings.HasSuffix(host, ".") {
		return errBadHostGlob
	}
	if _, err := path.Match(pattern, ""); err != nil {
		return err
	}
	return nil
}

// asciiHostGlob returns the host glob pattern with labels separated by ASCII
// dots, lower-cased, each Unicode label in its ASCII (punycode) form. A
// Unicode label holding a glob metacharacter has no ASCII form, and one IDNA
// would split further has no single label form: both are rejected.
func asciiHostGlob(pattern string) (string, error) {
	labels := strings.Split(unicodeDots.Replace(pattern), ".")
	for i, label := range labels {
		if isASCII(label) {
			labels[i] = strings.ToLower(label)
			continue
		}
		if strings.ContainsAny(label, `*?[]\`) {
			return "", fmt.Errorf("label %q mixes Unicode and glob metacharacters, write it in punycode", label)
		}
		ascii, err := idna.Lookup.ToASCII(label)
		if err != nil {
			return "", fmt.Errorf("label %q is not a valid internationalized name: %w", label, err)
		}
		if strings.Contains(ascii, ".") {
			return "", fmt.Errorf("label %q holds a label separator, write it with ASCII dots", label)
		}
		if ascii == "" {
			return "", fmt.Errorf("label %q maps to nothing", label)
		}
		labels[i] = ascii
	}
	return strings.Join(labels, "."), nil
}

// NormalizeHost returns a requested host name as rules match it: in lower
// case, for an internationalized name in the ASCII (punycode) form net/http
// dials, and then without a trailing dot — a Unicode full stop ending the
// name becomes one only in that form. A name with no ASCII form, which
// net/http cannot dial either, has its Unicode full stops read as dots and
// is lower-cased.
func NormalizeHost(host string) string {
	if !isASCII(host) {
		if ascii, err := idna.Lookup.ToASCII(host); err == nil {
			host = ascii
		} else {
			host = unicodeDots.Replace(host)
		}
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// isASCII reports whether s holds only ASCII bytes.
func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
