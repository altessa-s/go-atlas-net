// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package clientip

import (
	"context"
	"iter"
	"log/slog"
	"net/netip"
	"slices"
	"strings"

	"github.com/altessa-s/go-atlas-core/text/strings/intern"

	"github.com/altessa-s/go-atlas-net/netipx"

	coreerrs "github.com/altessa-s/go-atlas-core/errors"
	lru "github.com/hashicorp/golang-lru/v2"
)

// HeaderGetter abstracts multi-valued header access so that [Extractor] can
// work with both HTTP requests and gRPC incoming metadata without depending
// on either transport directly. The transport adapters
// httpx.NewHTTPHeaderGetter(r) and metadata.NewHeaderGetter(ctx) (package
// github.com/altessa-s/go-atlas-net/grpc/metadata) satisfy it implicitly.
type HeaderGetter interface {
	// GetHeader returns all values for the given header name.
	GetHeader(name string) []string
}

// Extractor resolves the real client IP from a request's peer address and
// proxy headers. It consults headers in a deterministic order, applies
// trusted-proxy and private-range filtering, and caches parsed addresses
// in an LRU to reduce allocation pressure under load.
//
// Safe for concurrent use; the underlying LRU cache is thread-safe.
type Extractor struct {
	opts    *options
	cache   *lru.Cache[string, netip.Addr]
	headers []string // Interned headers for performance
}

// NewExtractor creates a new [Extractor] configured with the given options.
// Returns an error if the LRU cache cannot be initialized (e.g. invalid size).
//
// Example:
//
//	extractor, err := clientip.NewExtractor(
//		clientip.WithTrustedProxies(netip.MustParsePrefix("10.0.0.0/8")),
//	)
func NewExtractor(opt ...Option) (*Extractor, error) {
	const mappedIPv4PrefixBits = 96

	opts := newOptions(opt...)
	// Addresses are unmapped before trust checks, so mapped IPv4 prefixes
	// must use the same representation. Clone to avoid changing caller slices.
	for _, prefixes := range []*[]netip.Prefix{&opts.trustedPeers, &opts.trustedProxies} {
		*prefixes = slices.Clone(*prefixes)
		for i, prefix := range *prefixes {
			if prefix.Addr().Is4In6() && prefix.Bits() >= mappedIPv4PrefixBits {
				(*prefixes)[i] = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-mappedIPv4PrefixBits)
			}
		}
	}

	var cache *lru.Cache[string, netip.Addr]

	// Only create cache if not disabled and size > 0
	if !opts.cacheDisabled && opts.cacheSize > 0 {
		var err error
		if cache, err = lru.New[string, netip.Addr](opts.cacheSize); err != nil {
			return nil, coreerrs.Wrapf(err, "failed to create IP cache, size=%d", opts.cacheSize)
		}
	}

	return &Extractor{
		opts:    opts,
		cache:   cache,
		headers: intern.StringSlice(opts.headers),
	}, nil
}

// debugEnabled reports whether debug logs are emitted. Debug calls are guarded
// so disabled logging does not format addresses on every extraction.
func (e *Extractor) debugEnabled(ctx context.Context) bool {
	return e.opts.logger.Enabled(ctx, slog.LevelDebug)
}

// Extract determines the real client IP using the following strategy:
//
//  1. If peerIP is not trusted — not in the trusted peers or trusted proxies,
//     and not private with [WithTrustPrivatePeers] set — it is returned
//     immediately: an untrusted peer cannot speak for anyone else.
//  2. Otherwise, the first configured proxy header present decides. For
//     X-Forwarded-For only explicitly trusted proxy hops are skipped, or a
//     nonzero trusted proxy count selects an exact position. Any other
//     header contributes only its rightmost entry.
//  3. A private or invalid value ends extraction with peerIP; later
//     headers are not consulted. If no configured header is present,
//     peerIP is returned as a fallback.
//
// Returns an invalid [netip.Addr] only when peerIP itself is invalid and
// no header yields a valid address.
func (e *Extractor) Extract(ctx context.Context, peerIP netip.Addr, headers HeaderGetter) netip.Addr {
	trusted := e.peerTrusted(peerIP.Unmap())

	if e.debugEnabled(ctx) {
		e.opts.logger.DebugContext(ctx, "peer IP",
			slog.String("ip", peerIP.String()),
			slog.Bool("is_private", netipx.IsPrivate(peerIP.Unmap())),
			slog.Bool("is_trusted", trusted),
		)
	}

	if !trusted {
		if e.debugEnabled(ctx) {
			e.opts.logger.DebugContext(ctx, "using peer IP as real IP", slog.String("ip", peerIP.String()))
		}
		return peerIP
	}

	// Process headers only if enabled
	if !e.opts.headersEnabled {
		if e.debugEnabled(ctx) {
			e.opts.logger.DebugContext(ctx, "headers processing disabled, falling back to peer IP",
				slog.String("ip", peerIP.String()))
		}
		return peerIP
	}

	// The first configured header present decides: a private, invalid or
	// unverifiable value ends extraction with peerIP instead of falling
	// through to another header the same client may have supplied.
	for _, header := range e.headers {
		ips := e.getIPsFromHeader(ctx, header, headers)
		if len(ips) == 0 {
			continue
		}

		if isForwardedFor(header) {
			if ip := e.parseXForwardedForHeader(ctx, ips); ip.IsValid() {
				if e.debugEnabled(ctx) {
					e.opts.logger.DebugContext(ctx, "found IP from X-Forwarded-For",
						slog.String("ip", ip.String()))
				}
				return ip
			}
			return peerIP
		}

		if addr, ok := e.parseAndCheckIP(ctx, ips[0], header); ok {
			return addr
		}
		return peerIP
	}

	// No configured header is present — fall back to the peer IP.
	// This handles local development (127.0.0.1, ::1) where no proxy headers are present.
	if e.debugEnabled(ctx) {
		e.opts.logger.DebugContext(ctx, "no IP from headers, falling back to peer IP",
			slog.String("ip", peerIP.String()))
	}

	return peerIP
}

// peerTrusted reports whether peer may supply forwarding headers: it is a
// configured trusted peer or trusted proxy, or it is private and private
// peers were opted in with [WithTrustPrivatePeers].
func (e *Extractor) peerTrusted(peer netip.Addr) bool {
	return netipx.IsTrusted(peer, e.opts.trustedPeers) ||
		netipx.IsTrusted(peer, e.opts.trustedProxies) ||
		(e.opts.trustPrivatePeers && netipx.IsPrivate(peer))
}

// isForwardedFor reports whether header names X-Forwarded-For. Header names
// are case-insensitive, and gRPC metadata keys are lowercase, so a configured
// "x-forwarded-for" must still get the right-to-left treatment.
func isForwardedFor(header string) bool {
	return strings.EqualFold(header, HeaderXForwardedFor)
}

// getIPsFromHeader returns the entries of header that may be consulted: the
// retained right end of an X-Forwarded-For chain, or the single rightmost
// entry of any other header. Single-address headers are overwritten or
// appended by the proxy that sets them, so the right end is the only entry the
// proxy vouches for; anything to its left is what the client sent.
func (e *Extractor) getIPsFromHeader(ctx context.Context, header string, headers HeaderGetter) []string {
	headerValues := headers.GetHeader(header)
	if len(headerValues) == 0 {
		return nil
	}

	if e.debugEnabled(ctx) {
		e.opts.logger.DebugContext(ctx, "processing header",
			slog.String("header", header), slog.Int("values_count", len(headerValues)))
	}

	var ips []string
	if isForwardedFor(header) {
		ips = forwardedForIPs(headerValues)
	} else {
		last := headerValues[len(headerValues)-1]
		entry := strings.TrimSpace(last[strings.LastIndexByte(last, ',')+1:])
		if len(entry) > MaxIPLength {
			entry = "" // Overlong: keep the header decisive, but never parse it.
		}
		ips = []string{entry}
	}

	if e.debugEnabled(ctx) {
		e.opts.logger.DebugContext(ctx, "ips from header", slog.String("header", header),
			slog.Any("ips", ips))
	}

	return ips
}

// forwardedForIPs returns the rightmost [MaxIPsPerHeader] entries of an
// X-Forwarded-For chain in left-to-right order. Each proxy appends to the
// right, so the trusted suffix lives there and the left end is whatever the
// client sent: collecting from the left would let a client pad the chain until
// the proxy-appended entry falls off the limit. Field lines are consumed last
// to first and each one is split from its right end, so only the consumed
// tail is scanned however long the header is. Empty and overlong entries keep
// their positions as empty placeholders so proxy counts cannot shift left.
func forwardedForIPs(values []string) []string {
	ips := make([]string, 0, MaxIPsPerHeader)
	parts := 0
	for _, value := range slices.Backward(values) {
		for parts < MaxIPsPerHeader {
			i := strings.LastIndexByte(value, ',')
			parts++
			if trimmed := strings.TrimSpace(value[i+1:]); trimmed != "" && len(trimmed) <= MaxIPLength {
				ips = append(ips, trimmed)
			} else {
				ips = append(ips, "")
			}
			if i < 0 {
				break
			}
			value = value[:i]
		}
		if parts >= MaxIPsPerHeader {
			break
		}
	}
	slices.Reverse(ips)
	return ips
}

func (e *Extractor) parseAndCheckIP(ctx context.Context, ip, header string) (netip.Addr, bool) {
	// Check cache first if available
	if e.cache != nil {
		if cachedAddr, ok := e.cache.Get(ip); ok {
			if e.debugEnabled(ctx) {
				e.opts.logger.DebugContext(ctx, "found in cache",
					slog.String("ip", cachedAddr.String()),
					slog.Bool("is_private", netipx.IsPrivate(cachedAddr)))
			}

			if !netipx.IsPrivate(cachedAddr) {
				return cachedAddr, true
			}

			return netip.Addr{}, false
		}
	}

	addr, err := parseAddr(ip)
	if err != nil {
		e.opts.logger.WarnContext(ctx, "failed to parse IP",
			slog.String("ip", ip),
			slog.String("error", err.Error()))
		return netip.Addr{}, false
	}

	e.cachePut(ip, addr)

	if e.debugEnabled(ctx) {
		e.opts.logger.DebugContext(ctx, "parsed IP",
			slog.String("ip", addr.String()),
			slog.Bool("is_private", netipx.IsPrivate(addr)))
	}

	if !netipx.IsPrivate(addr) {
		if e.debugEnabled(ctx) {
			e.opts.logger.DebugContext(ctx, "found public IP",
				slog.String("ip", addr.String()),
				slog.String("header", header))
		}
		return addr, true
	}

	return netip.Addr{}, false
}

// parseAddr parses a header entry and unmaps an IPv4-mapped IPv6 address, so
// "::ffff:10.0.0.1" is classified, cached, and reported as the IPv4 address it
// carries instead of slipping past the IPv4 private and trusted prefixes.
func parseAddr(s string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(s)
	return addr.Unmap(), err
}

// Headers returns an iterator over the configured header names in their
// evaluation order.
func (e *Extractor) Headers() iter.Seq[string] {
	return slices.Values(e.headers)
}

// parseXForwardedForHeader parses X-Forwarded-For header values.
func (e *Extractor) parseXForwardedForHeader(ctx context.Context, ips []string) netip.Addr {
	if len(ips) == 0 {
		return netip.Addr{}
	}

	if e.debugEnabled(ctx) {
		e.opts.logger.DebugContext(ctx, "parsing X-Forwarded-For",
			slog.Any("ips", ips),
			slog.Uint64("trusted_proxies_count", uint64(e.opts.trustedProxiesCount)))
	}

	// X-Forwarded-For format: client, proxy1, proxy2, ...
	// We need to find the rightmost IP that is not from a trusted proxy

	// If we have configured trusted proxies count, we can trust that many proxies from the right
	if e.opts.trustedProxiesCount > 0 {
		return e.xffByProxyCount(ctx, ips)
	}
	return e.xffRightmostUntrusted(ips)
}

// xffByProxyCount returns the X-Forwarded-For entry just left of the
// configured number of trusted proxies, when it is a public address.
func (e *Extractor) xffByProxyCount(ctx context.Context, ips []string) netip.Addr {
	if uint(len(ips)) <= e.opts.trustedProxiesCount {
		return netip.Addr{}
	}
	index := len(ips) - int(e.opts.trustedProxiesCount) - 1 // #nosec G115 -- proxy count is small
	if index < 0 || index >= len(ips) {
		return netip.Addr{}
	}
	ip := ips[index]
	if e.debugEnabled(ctx) {
		e.opts.logger.DebugContext(ctx, "checking IP by trusted proxy count",
			slog.String("ip", ip),
			slog.Int("index", index))
	}

	addr, err := parseAddr(ip)
	if err != nil || netipx.IsPrivate(addr) {
		return netip.Addr{}
	}
	if e.debugEnabled(ctx) {
		e.opts.logger.DebugContext(ctx, "found IP by trusted proxy count",
			slog.String("ip", addr.String()))
	}
	return addr
}

// xffRightmostUntrusted walks the X-Forwarded-For entries from the right:
// only explicitly trusted proxies may vouch for the preceding hop, so the
// first entry that is not a trusted proxy is the client when it is public.
func (e *Extractor) xffRightmostUntrusted(ips []string) netip.Addr {
	for _, ip := range slices.Backward(ips) {
		if len(ip) > MaxIPLength {
			return netip.Addr{}
		}
		addr, ok := e.xffAddr(ip)
		if !ok {
			return netip.Addr{}
		}
		if netipx.IsTrusted(addr, e.opts.trustedProxies) {
			continue
		}
		if netipx.IsPrivate(addr) {
			return netip.Addr{}
		}

		// Found a public, untrusted IP - this is likely the real client IP
		return addr
	}

	// The retained chain contains only trusted proxies and names no client.
	// Use the connection peer instead of accepting an unverifiable claim.
	return netip.Addr{}
}

// xffAddr parses an X-Forwarded-For entry through the address cache, if
// any; it reports false for an entry that is not an address.
func (e *Extractor) xffAddr(ip string) (netip.Addr, bool) {
	if e.cache != nil {
		if cachedAddr, ok := e.cache.Get(ip); ok {
			return cachedAddr, true
		}
	}

	addr, err := parseAddr(ip)
	if err != nil {
		return netip.Addr{}, false
	}

	e.cachePut(ip, addr)
	return addr, true
}

// cachePut stores a parsed entry, if the cache is enabled. Header entries are
// client-written substrings of the request's header values, so they are never
// interned process-wide, and the cache keeps its own copy of the key instead
// of pinning the whole header for as long as the entry lives.
func (e *Extractor) cachePut(ip string, addr netip.Addr) {
	if e.cache != nil {
		e.cache.Add(strings.Clone(ip), addr)
	}
}
