// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

// Package clientip extracts the real client IP address from HTTP and gRPC
// requests by traversing proxy header chains.
//
// When a request arrives through one or more reverse proxies, the direct
// peer address belongs to the last proxy, not the original client.
// [Extractor] consults the configured headers in order, and the first one
// present decides. The default is X-Forwarded-For only, walked right-to-left,
// skipping only explicitly trusted proxy hops. Single-address headers
// (X-Real-IP, CF-Connecting-IP, True-Client-IP, ...) are opt-in through
// [WithHeaders] and contribute only their rightmost entry: a client can send
// any of them, so list one only when the trusted proxy always sets it. A
// private or invalid value stops extraction with the peer address; earlier
// entries and later headers cannot override it.
// Headers are honored only when the connection peer is explicitly trusted
// through [WithTrustedPeers] or [WithTrustedProxies]. Private and loopback
// peers are not automatically trusted; [WithTrustPrivatePeers] restores that
// legacy behavior when explicitly requested.
//
// Parsed addresses are cached in an LRU to avoid repeated [netip.ParseAddr]
// calls on hot paths. The cache, trusted-proxy list, and header set are all
// configurable through functional [Option] values.
//
// Use [NewContext] and [FromContext] to propagate the resolved IP through
// request contexts.
//
// The package is transport-agnostic: [Extractor] reads headers through the
// consumer-side [HeaderGetter] interface. HTTP callers pass
// httpx.NewHTTPHeaderGetter(r) and gRPC callers pass
// metadata.NewHeaderGetter(ctx) from the grpc/metadata package; both
// satisfy [HeaderGetter] implicitly.
//
// Example:
//
//	extractor, _ := clientip.NewExtractor(
//	    clientip.WithTrustedProxies(netip.MustParsePrefix("10.0.0.0/8")),
//	    clientip.WithHeaders("CF-Connecting-IP", "X-Forwarded-For"),
//	)
//	ip := extractor.Extract(r.Context(), peerIP, httpx.NewHTTPHeaderGetter(r))
//	ctx = clientip.NewContext(r.Context(), ip)
package clientip
