// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package proxydial

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
)

// AddrGuard decides whether a proxied destination may be reached. host is
// the host part of the dialed "host:port" (a name or an IP literal) and dst
// the resolved address with the destination port; the address is unmapped
// (IPv4-mapped IPv6 becomes IPv4) and has no zone. A non-nil error refuses
// the dial and is returned wrapped, so [errors.Is] and [errors.As] still
// match it. httpclient.EgressPolicy.Check is an AddrGuard whose refusals are
// *httpclient.SSRFError values matching httpclient.ErrSSRFBlocked.
type AddrGuard func(host string, dst netip.AddrPort) error

// Resolver looks up the IP addresses of a host name. *[net.Resolver]
// satisfies it.
type Resolver interface {
	// LookupNetIP looks up host for network "ip", "ip4" or "ip6".
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// guardDial wraps dial so every destination is resolved locally, checked
// with guard, and handed to dial as an approved "ip:port". It resolves once:
// the address that is checked is the address the proxy is asked to connect
// to. When any resolved address is refused the dial fails without contacting
// the proxy, since a name that resolves to both public and private addresses
// is a rebinding signal, not a fallback opportunity. Approved addresses are
// tried in resolver order until one connects; the errors of the failed
// attempts are joined.
func guardDial(dial DialContextFunc, guard AddrGuard, resolver Resolver) DialContextFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		targets, err := approvedTargets(ctx, network, addr, guard, resolver)
		if err != nil {
			return nil, err
		}
		var errs []error
		for _, target := range targets {
			conn, dialErr := dial(ctx, network, target.String())
			if dialErr == nil {
				return conn, nil
			}
			errs = append(errs, dialErr)
			if ctx.Err() != nil {
				break
			}
		}
		return nil, errors.Join(errs...)
	}
}

// approvedTargets resolves the host of addr (an IP literal is used as is)
// and returns the resolved addresses with the port of addr when guard
// approves every one of them.
func approvedTargets(ctx context.Context, network, addr string, guard AddrGuard, resolver Resolver) ([]netip.AddrPort, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("proxy destination %q: %w", addr, err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("proxy destination %q: invalid port: %w", addr, err)
	}

	var ips []netip.Addr
	if ip, perr := netip.ParseAddr(host); perr == nil {
		ips = []netip.Addr{ip}
	} else {
		ips, err = resolver.LookupNetIP(ctx, lookupNetwork(network), host)
		if err != nil {
			return nil, fmt.Errorf("resolve proxy destination %s: %w", host, err)
		}
		if len(ips) == 0 {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
	}

	targets := make([]netip.AddrPort, 0, len(ips))
	for _, ip := range ips {
		dst := netip.AddrPortFrom(ip.Unmap().WithZone(""), uint16(port))
		if err := guard(host, dst); err != nil {
			return nil, fmt.Errorf("proxy destination %s refused: %w", host, err)
		}
		targets = append(targets, dst)
	}
	return targets, nil
}

// lookupNetwork maps a dial network to the matching lookup network.
func lookupNetwork(network string) string {
	switch network {
	case "tcp4":
		return "ip4"
	case "tcp6":
		return "ip6"
	default:
		return "ip"
	}
}
