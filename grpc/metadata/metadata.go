// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package metadata

import (
	"context"

	grpcmd "google.golang.org/grpc/metadata"
)

// HeaderGetter is a provider-side adapter over the incoming gRPC metadata of
// a [context.Context]. It satisfies consumer-side header interfaces
// implicitly: multi-value getters such as clientip.HeaderGetter through
// [HeaderGetter.GetHeader], and single-value getters through
// [HeaderGetter.GetSingleHeader]. Header names are automatically lowercased
// by gRPC metadata conventions.
type HeaderGetter struct {
	ctx context.Context
}

// NewHeaderGetter creates a new HeaderGetter from a context.
//
// Example:
//
//	adapter := metadata.NewHeaderGetter(ctx)
//	forwardedFor := adapter.GetHeader("x-forwarded-for")
func NewHeaderGetter(ctx context.Context) *HeaderGetter {
	return &HeaderGetter{ctx: ctx}
}

// GetHeader returns all values for the given header name from gRPC metadata.
// This method is compatible with interfaces that expect []string.
//
// Example:
//
//	values := adapter.GetHeader("x-forwarded-for")
func (g *HeaderGetter) GetHeader(name string) []string {
	if g == nil || g.ctx == nil {
		return nil
	}
	return grpcmd.ValueFromIncomingContext(g.ctx, name)
}

// GetSingleHeader returns the first value for the given header name from gRPC metadata.
// This method is compatible with interfaces that expect string.
//
// Example:
//
//	requestID := adapter.GetSingleHeader("x-request-id")
func (g *HeaderGetter) GetSingleHeader(name string) string {
	if g == nil || g.ctx == nil {
		return ""
	}
	if vals := grpcmd.ValueFromIncomingContext(g.ctx, name); len(vals) > 0 {
		return vals[0]
	}
	return ""
}
