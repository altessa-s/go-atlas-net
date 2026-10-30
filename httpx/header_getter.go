// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package httpx

import "net/http"

// HTTPHeaderGetter is a provider-side adapter over an [http.Request]. It
// satisfies consumer-side header interfaces implicitly: multi-value getters
// such as clientip.HeaderGetter through [HTTPHeaderGetter.GetHeader], and
// single-value getters through [HTTPHeaderGetter.GetSingleHeader]. It is
// safe for concurrent reads when the underlying request is not mutated.
type HTTPHeaderGetter struct {
	request *http.Request
}

// NewHTTPHeaderGetter creates a new HTTPHeaderGetter from an http.Request.
// Returns nil if the request is nil.
//
// Example:
//
//	adapter := httpx.NewHTTPHeaderGetter(request)
//	contentType := adapter.GetSingleHeader("Content-Type")
func NewHTTPHeaderGetter(r *http.Request) *HTTPHeaderGetter {
	if r == nil {
		return nil
	}
	return &HTTPHeaderGetter{request: r}
}

// GetHeader returns all values for the given header name.
// This method is compatible with interfaces that expect []string.
//
// Example:
//
//	values := adapter.GetHeader("X-Forwarded-For")
func (h *HTTPHeaderGetter) GetHeader(name string) []string {
	if h == nil || h.request == nil {
		return nil
	}
	return h.request.Header.Values(name)
}

// GetSingleHeader returns the first value for the given header name.
// This method is compatible with interfaces that expect string.
//
// Example:
//
//	requestID := adapter.GetSingleHeader("X-Request-ID")
func (h *HTTPHeaderGetter) GetSingleHeader(name string) string {
	if h == nil || h.request == nil {
		return ""
	}
	return h.request.Header.Get(name)
}
