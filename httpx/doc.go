// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

// Package httpx holds small net/http extensions shared across the module:
// function adapters (Doer, DoerFunc, RoundTripperFunc), header name
// constants, status classes and the request header adapter
// (HTTPHeaderGetter).
//
// # Scope
//
// The package deliberately holds no client, IP or policy logic: the outbound
// client lives in http/client, client IP resolution in clientip and address
// policy in netipx and egressrule. Keep additions to stateless helpers that
// several of those packages need.
//
// # Header names
//
// ContentType, Accept, RetryAfter, RateLimitLimit and the other header name
// variables are interned at package init so the same string is shared by
// every request; the Header* constants hold the raw values. HTTPHeaderGetter
// adapts an [*net/http.Request] to consumer-side header getter interfaces
// such as clientip.HeaderGetter; its gRPC counterpart is
// grpc/metadata.HeaderGetter.
//
// # Adapters
//
// RoundTripperFunc lets a function implement [net/http.RoundTripper]. Doer is
// the one-method interface satisfied by [*net/http.Client]; code that only
// sends requests should accept a Doer, and DoerFunc lets a function implement
// it (typically a test double). Both adapters are stateless and safe for
// concurrent use when the wrapped function is.
//
// # Status classes
//
// IsSuccess and IsRedirect report whether a status code is in the 2xx and 3xx
// class respectively.
//
// # Usage
//
//	import "github.com/altessa-s/go-atlas-net/httpx"
//
//	rt := httpx.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
//	    // Clone before mutating: RoundTrippers must not modify the request.
//	    req = req.Clone(req.Context())
//	    req.Header.Set("User-Agent", "example/1.0")
//	    return http.DefaultTransport.RoundTrip(req)
//	})
//
//	var doer httpx.Doer = &http.Client{Transport: rt}
//	resp, err := doer.Do(req)
//	if err != nil {
//	    return err
//	}
//	defer resp.Body.Close()
//	if !httpx.IsSuccess(resp.StatusCode) {
//	    return fmt.Errorf("unexpected status %d", resp.StatusCode)
//	}
package httpx
