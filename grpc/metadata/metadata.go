// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package metadata

import (
	"context"

	grpcmd "google.golang.org/grpc/metadata"
)

// HeaderGetter reads incoming gRPC metadata from a [context.Context],
// adapting it to the same header getter interfaces used by HTTP adapters.
// Header names are automatically lowercased by gRPC metadata conventions.
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
