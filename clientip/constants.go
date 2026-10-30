// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package clientip

import (
	"github.com/altessa-s/go-atlas-core/text/strings/intern"
)

// Constants for IP processing limits.
const (
	// MaxIPsPerHeader is the maximum IPs to process from a single header.
	MaxIPsPerHeader = 10

	// MaxIPLength is the maximum IP string length (IPv6 max).
	MaxIPLength = 45
)

// Header constants for common proxy headers.
const (
	// HeaderXForwardedFor is the X-Forwarded-For header.
	HeaderXForwardedFor = "X-Forwarded-For"

	// HeaderXRealIP is the X-Real-IP header.
	HeaderXRealIP = "X-Real-IP"

	// HeaderXClientIP is the X-Client-IP header.
	HeaderXClientIP = "X-Client-IP"

	// HeaderCFConnectingIP is the Cloudflare CF-Connecting-IP header.
	HeaderCFConnectingIP = "CF-Connecting-IP"

	// HeaderFastlyClientIP is the Fastly-Client-Ip header.
	HeaderFastlyClientIP = "Fastly-Client-Ip"

	// HeaderTrueClientIP is the True-Client-Ip header.
	HeaderTrueClientIP = "True-Client-Ip"
)

// Interned header names for memory efficiency.
// These deduplicated strings avoid per-request allocations when the same
// header name is used across many concurrent extractions.
var (
	InternedHeaderXForwardedFor  = intern.String(HeaderXForwardedFor)
	InternedHeaderXRealIP        = intern.String(HeaderXRealIP)
	InternedHeaderXClientIP      = intern.String(HeaderXClientIP)
	InternedHeaderCFConnectingIP = intern.String(HeaderCFConnectingIP)
	InternedHeaderFastlyClientIP = intern.String(HeaderFastlyClientIP)
	InternedHeaderTrueClientIP   = intern.String(HeaderTrueClientIP)
)

// DefaultHeaders returns the default proxy header list: X-Forwarded-For
// only. Single-address headers (X-Real-IP, CF-Connecting-IP, True-Client-IP,
// …) are opt-in through [WithHeaders]: a client can send any of them, so only
// a header the trusted proxy is known to set may be consulted.
func DefaultHeaders() []string {
	return []string{InternedHeaderXForwardedFor}
}
