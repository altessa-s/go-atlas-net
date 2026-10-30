// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"syscall"

	"github.com/altessa-s/go-atlas-net/egressrule"
	"github.com/altessa-s/go-atlas-net/netipx"
)

// ErrInvalidEgressRule is returned by [NewEgressPolicy] for a rule that
// cannot be compiled: a deny rule without matchers, an allow rule without a
// CIDR, a malformed host glob or an invalid prefix.
var ErrInvalidEgressRule = errors.New("httpclient: invalid egress rule")

// Default destination ports of the http and https schemes.
const (
	defaultHTTPPort  = 80
	defaultHTTPSPort = 443
)

// schemeHTTPS is the URL scheme of HTTP over TLS.
const schemeHTTPS = "https"

// EgressRule matches an outbound destination. Every matcher that is set must
// match (AND); a zero matcher matches anything.
type EgressRule struct {
	// Host is a case-insensitive glob in [path.Match] syntax, matched against
	// the host name the connection was requested for (the URL host, or the
	// proxy host when dialing a proxy) without a trailing dot. "*" also
	// matches dots: "*.example.com" matches "a.b.example.com" but not
	// "example.com". A literal IP host is matched in its text form. An
	// internationalized name matches in its ASCII (punycode) form, the one
	// net/http dials: a Unicode label of the glob is converted to it
	// ("bücher.example" matches "xn--bcher-kva.example" and "bücher.example"),
	// and a Unicode label holding a glob metacharacter is rejected; write it in
	// punycode. The Unicode full stops IDNA reads as dots (U+3002, U+FF0E,
	// U+FF61) separate labels in rules and hosts alike; a rule may not end in
	// one.
	Host string
	// CIDR matches the resolved IP address. A connection to an IPv4-mapped
	// IPv6 address (::ffff:a.b.c.d) is made over IPv4, so such an address is
	// matched as the IPv4 address only: an IPv6 prefix, ::/0 included,
	// matches IPv6 connections only. An IPv4-mapped prefix of length 96 or
	// more is the equivalent IPv4 prefix; a shorter one is rejected, since it
	// would span IPv4-mapped and IPv6 space.
	CIDR netip.Prefix
	// Port matches the destination port.
	Port uint16
	// Name identifies the rule in decisions and refusals (the label of
	// [EgressDecision].Rule and [SSRFError].Rule), followed by its
	// matchers. Empty names it by its list and position ("deny[0]"). It
	// does not take part in matching; errors of [NewEgressPolicy] always
	// name the position.
	Name string
}

// EgressPolicy is a compiled allow/deny policy for outbound connections. A
// destination is refused when any deny rule matches it (deny wins); else it
// is allowed when any allow rule matches it; else it is refused when its
// resolved address is private, loopback, link-local or otherwise reserved
// ([netipx.IsPrivate]) and allowed otherwise. Allow rules therefore only
// exempt addresses from the default private denial, which is why each one
// needs a CIDR; host and port narrow it. Every refusal is an [*SSRFError]
// matching [ErrSSRFBlocked]; [EgressPolicy.Explain] reports the decision
// with its reason and rule.
//
// IPv4-mapped IPv6 addresses are decided as the IPv4 address they connect
// to (net.Dialer and proxydial both reach them over IPv4), so a policy
// decides an address the same way whichever form a resolver or a URL gave
// it in; zones are ignored. An
// EgressPolicy is immutable and safe for concurrent use. Build it with
// [NewEgressPolicy] and install it with [WithEgressPolicy], with
// [EgressPolicy.DialContext] in a caller-built transport, or as a
// proxydial.AddrGuard through [EgressPolicy.Check].
type EgressPolicy struct {
	allow []egressRule
	deny  []egressRule
}

// egressRule is a validated, normalized [EgressRule].
type egressRule struct {
	host  string       // lower-case glob; "" matches any host
	cidr  netip.Prefix // invalid matches any address
	port  uint16       // 0 matches any port
	label string       // "deny[0] host=… cidr=… port=…", reported in SSRFError.Rule and EgressDecision.Rule
}

// EgressReason is why an [EgressPolicy] decided a destination as it did.
type EgressReason int

// Egress decision reasons.
const (
	// EgressInvalidAddress refuses a destination whose address is not valid.
	EgressInvalidAddress EgressReason = iota + 1
	// EgressDenyRule refuses a destination a deny rule matches.
	EgressDenyRule
	// EgressAllowRule allows a destination an allow rule matches.
	EgressAllowRule
	// EgressPrivateDefault refuses a private, loopback, link-local or
	// reserved address no rule matches.
	EgressPrivateDefault
	// EgressPublic allows a public address no rule matches.
	EgressPublic
)

// egressReasonNames are the log names of the reasons, by value.
var egressReasonNames = [...]string{
	EgressInvalidAddress: "invalid_address",
	EgressDenyRule:       "deny_rule",
	EgressAllowRule:      "allow_rule",
	EgressPrivateDefault: "private_default",
	EgressPublic:         "public",
}

// String returns the reason in snake case ("deny_rule"), for logs, or
// "EgressReason(n)" for a value outside the constants.
func (r EgressReason) String() string {
	if r > 0 && int(r) < len(egressReasonNames) {
		return egressReasonNames[r]
	}
	return "EgressReason(" + strconv.Itoa(int(r)) + ")"
}

// EgressDecision is the outcome of [EgressPolicy.Explain].
type EgressDecision struct {
	// Allowed reports whether the destination may be reached.
	Allowed bool
	// Reason is why it was decided so.
	Reason EgressReason
	// Rule is the label of the matching rule for EgressDenyRule and
	// EgressAllowRule ("deny[0] host=*.internal", "allowedCIDRs[1] cidr=…",
	// or the rule's Name), empty otherwise.
	Rule string
	// Host is the host name the destination was requested for.
	Host string
	// IP is the decided address: zone-free, an IPv4-mapped address as IPv4;
	// invalid for EgressInvalidAddress.
	IP netip.Addr
}

// Err returns nil for an allowed destination and the [*SSRFError] of a
// refused one (its Rule set for a deny rule).
func (d EgressDecision) Err() error {
	if d.Allowed {
		return nil
	}
	e := &SSRFError{Host: d.Host, Rule: d.Rule}
	if d.IP.IsValid() {
		e.IP = d.IP.String()
	}
	return e
}

// NewEgressPolicy compiles allow and deny rules into an [EgressPolicy]. A
// deny rule needs at least one matcher, an allow rule needs a CIDR, a host
// glob must be a valid [path.Match] pattern without a scheme, port, path,
// space or trailing dot, and a non-zero CIDR must be valid
// ([egressrule.Check], the grammar configuration schemas validate with).
// Errors wrap [ErrInvalidEgressRule] and name the rule ("deny[1].host").
// With no rules the policy enforces only the default private denial. The
// rule slices are copied.
func NewEgressPolicy(allow, deny []EgressRule) (*EgressPolicy, error) {
	p := &EgressPolicy{
		allow: make([]egressRule, 0, len(allow)),
		deny:  make([]egressRule, 0, len(deny)),
	}
	for i, r := range deny {
		m, err := egressrule.Check(egressrule.Deny, i, matcherOf(r))
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidEgressRule, err)
		}
		p.deny = append(p.deny, compileEgressRule(egressrule.Deny, i, m))
	}
	for i, r := range allow {
		m, err := egressrule.Check(egressrule.Allow, i, matcherOf(r))
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidEgressRule, err)
		}
		p.allow = append(p.allow, compileEgressRule(egressrule.Allow, i, m))
	}
	return p, nil
}

// ParseEgressPolicy compiles rules in their textual form, as configured or
// stored ([egressrule.Rule]), into an [EgressPolicy], as [NewEgressPolicy]
// does: it also rejects a CIDR that does not parse and a port outside 0 to
// 65535 ([egressrule.Rule.Parse]). Errors wrap [ErrInvalidEgressRule] and
// name the rule ("allow[0].cidr").
func ParseEgressPolicy(allow, deny []egressrule.Rule) (*EgressPolicy, error) {
	p := &EgressPolicy{
		allow: make([]egressRule, 0, len(allow)),
		deny:  make([]egressRule, 0, len(deny)),
	}
	for i, r := range deny {
		m, err := r.Parse(egressrule.Deny, i)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidEgressRule, err)
		}
		p.deny = append(p.deny, compileEgressRule(egressrule.Deny, i, m))
	}
	for i, r := range allow {
		m, err := r.Parse(egressrule.Allow, i)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidEgressRule, err)
		}
		p.allow = append(p.allow, compileEgressRule(egressrule.Allow, i, m))
	}
	return p, nil
}

// matcherOf returns the matchers of r.
func matcherOf(r EgressRule) egressrule.Matcher {
	return egressrule.Matcher{Name: r.Name, Host: r.Host, CIDR: r.CIDR, Port: r.Port}
}

// compileEgressRule returns the compiled form of the checked, normalized
// matcher m of rule index of list, labeled by its name or its position.
func compileEgressRule(list egressrule.List, index int, m egressrule.Matcher) egressRule {
	c := egressRule{host: m.Host, cidr: m.CIDR, port: m.Port}
	name := m.Name
	if name == "" {
		name = fmt.Sprintf("%s[%d]", list, index)
	}
	c.label = egressRuleLabel(name, c)
	return c
}

func egressRuleLabel(name string, r egressRule) string {
	var b strings.Builder
	b.WriteString(name)
	if r.host != "" {
		b.WriteString(" host=")
		b.WriteString(r.host)
	}
	if r.cidr.IsValid() {
		b.WriteString(" cidr=")
		b.WriteString(r.cidr.String())
	}
	if r.port != 0 {
		b.WriteString(" port=")
		b.WriteString(strconv.Itoa(int(r.port)))
	}
	return b.String()
}

// matches reports whether every matcher of r matches. host is already
// normalized; ip is the zone-free, unmapped address, invalid when the
// address is not known yet.
func (r *egressRule) matches(host string, ip netip.Addr, port uint16) bool {
	if r.port != 0 && r.port != port {
		return false
	}
	if r.cidr.IsValid() {
		if !ip.IsValid() || !r.cidr.Contains(ip) {
			return false
		}
	}
	if r.host != "" {
		matched, _ := path.Match(r.host, host) // the pattern was validated by egressrule.Check
		return matched
	}
	return true
}

// NormalizeEgressHostGlob validates an [EgressRule] host glob and returns it in
// the form [NewEgressPolicy] compiles and matches it in
// ([egressrule.NormalizeHostGlob]): lower case, labels separated by ASCII
// dots (the Unicode full stops U+3002, U+FF0E and U+FF61 read as dots), each
// Unicode label in its ASCII (punycode) form under the IDNA lookup profile.
// A rejected glob returns an error wrapping [ErrInvalidEgressRule]. Callers
// that hand the rules on (a proxy, a browser) use it to send the exact form
// the policy matches.
func NormalizeEgressHostGlob(pattern string) (string, error) {
	host, err := egressrule.NormalizeHostGlob(pattern)
	if err != nil {
		return "", fmt.Errorf("%w: host %q: %w", ErrInvalidEgressRule, pattern, err)
	}
	return host, nil
}

// Check decides whether host, resolved to dst, may be reached. host is the
// name the connection was requested for (for host-glob rules); dst is the
// resolved address and destination port. It returns nil when the policy
// allows the destination and an [*SSRFError] otherwise; for a deny rule the
// error's Rule names it. An invalid address is refused. Its signature is that
// of proxydial.AddrGuard, so p.Check can guard proxied dials. It is
// [EgressPolicy.Explain] without the decision's details.
func (p *EgressPolicy) Check(host string, dst netip.AddrPort) error {
	return p.Explain(host, dst).Err()
}

// Explain decides whether host, resolved to dst, may be reached, as
// [EgressPolicy.Check] does, and reports why: the deny rule that refuses it,
// the allow rule that admits it, the default private denial, a public
// address, or an invalid one. An IPv4-mapped address is decided as the IPv4
// address it connects to.
func (p *EgressPolicy) Explain(host string, dst netip.AddrPort) EgressDecision {
	ip := dst.Addr().WithZone("").Unmap()
	if !ip.IsValid() {
		return EgressDecision{Reason: EgressInvalidAddress, Host: host}
	}
	normHost := egressrule.NormalizeHost(host)
	for i := range p.deny {
		if p.deny[i].matches(normHost, ip, dst.Port()) {
			return EgressDecision{Reason: EgressDenyRule, Rule: p.deny[i].label, Host: host, IP: ip}
		}
	}
	for i := range p.allow {
		if p.allow[i].matches(normHost, ip, dst.Port()) {
			return EgressDecision{Allowed: true, Reason: EgressAllowRule, Rule: p.allow[i].label, Host: host, IP: ip}
		}
	}
	if netipx.IsPrivate(ip) {
		return EgressDecision{Reason: EgressPrivateDefault, Host: host, IP: ip}
	}
	return EgressDecision{Allowed: true, Reason: EgressPublic, Host: host, IP: ip}
}

// CheckHost applies the policy to a destination before its host is resolved,
// such as a URL about to be requested or a target a proxy resolves: a host
// name is refused by the deny rules without a CIDR that match its name and
// port (its address is decided once it is resolved), and a literal IP host,
// in any form net/http sends ("127。0。0。1" included), is checked in full by
// [EgressPolicy.Check]. It returns nil or an [*SSRFError] (Rule naming a
// deny rule).
func (p *EgressPolicy) CheckHost(host string, port uint16) error {
	normHost := egressrule.NormalizeHost(host) // "127。0。0。1" is the literal IP net/http sends
	if ip, err := netip.ParseAddr(normHost); err == nil {
		return p.Check(host, netip.AddrPortFrom(ip, port))
	}
	for i := range p.deny {
		if !p.deny[i].cidr.IsValid() && p.deny[i].matches(normHost, netip.Addr{}, port) {
			return &SSRFError{Host: host, Rule: p.deny[i].label}
		}
	}
	return nil
}

// DialContext returns a dial function that applies p to every connection: it
// dials with a copy of d whose control hook checks each resolved address,
// after DNS resolution and before the connect, against the host and port of
// the dialed "host:port". The address that is checked is the one connected
// to, so there is no second resolution to rebind. When the host resolves to
// several addresses, a refused one fails like an unreachable one: the dialer
// moves on to the next address, and when none connects it returns the error
// of the first (the [*SSRFError] when that one was refused). A hook already
// set on d (Control or ControlContext) runs after the policy check; d itself
// is not modified. Use it in a transport the client does not build;
// [WithEgressPolicy] installs it in the client's own transport.
func (p *EgressPolicy) DialContext(d *net.Dialer) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		dialer := *d
		dialer.Control = nil
		dialer.ControlContext = p.control(host, d.Control, d.ControlContext)
		return dialer.DialContext(ctx, network, address)
	}
}

// control returns the dialer hook checking the resolved address of host and
// then running the caller's own hook, if any.
func (p *EgressPolicy) control(
	host string,
	next func(network, address string, c syscall.RawConn) error,
	nextCtx func(ctx context.Context, network, address string, c syscall.RawConn) error,
) func(ctx context.Context, network, address string, c syscall.RawConn) error {
	return func(ctx context.Context, network, address string, c syscall.RawConn) error {
		dst, err := netip.ParseAddrPort(address)
		if err != nil {
			return err
		}
		if err := p.Check(host, dst); err != nil {
			return err
		}
		switch {
		case nextCtx != nil:
			return nextCtx(ctx, network, address, c)
		case next != nil:
			return next(network, address, c)
		default:
			return nil
		}
	}
}

// withAllowedCIDRs returns p extended with an allow rule per prefix, the
// [WithSSRFAllowedCIDRs] exemptions. It returns p itself when there are none.
func (p *EgressPolicy) withAllowedCIDRs(prefixes []netip.Prefix) *EgressPolicy {
	if len(prefixes) == 0 {
		return p
	}
	out := &EgressPolicy{deny: p.deny, allow: make([]egressRule, 0, len(p.allow)+len(prefixes))}
	out.allow = append(out.allow, p.allow...)
	for i, prefix := range prefixes {
		if !prefix.IsValid() {
			continue
		}
		r := egressRule{cidr: egressrule.NormalizePrefix(prefix)}
		r.label = egressRuleLabel(fmt.Sprintf("allowedCIDRs[%d]", i), r)
		out.allow = append(out.allow, r)
	}
	return out
}

// guardProxy wraps proxy so a request whose target is refused by p fails
// with an [*SSRFError] instead of being sent through the proxy. The proxy
// resolves host names, so a host name target is checked only against the
// deny rules without a CIDR; a literal IP target is checked in full. Use
// proxydial with [EgressPolicy.Check] as its guard to resolve targets locally.
func (p *EgressPolicy) guardProxy(proxy ProxyFunc) ProxyFunc {
	return func(req *http.Request) (*url.URL, error) {
		u, err := proxy(req)
		if err != nil || u == nil {
			return u, err
		}
		if cerr := p.CheckHost(req.URL.Hostname(), targetPort(req.URL)); cerr != nil {
			return nil, cerr
		}
		return u, nil
	}
}

// targetPort returns the explicit port of u, or the default port of its
// scheme; 0 when the port is not a valid number.
func targetPort(u *url.URL) uint16 {
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil {
			return 0
		}
		return uint16(n)
	}
	if u.Scheme == schemeHTTPS {
		return defaultHTTPSPort
	}
	return defaultHTTPPort
}
