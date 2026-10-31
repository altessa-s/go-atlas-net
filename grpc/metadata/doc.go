// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

// Package metadata adapts incoming gRPC metadata to the header-access
// interface shared across transports ([HeaderGetter]), so protocol-neutral
// packages such as net/clientip read gRPC headers the same way they read
// HTTP ones.
//
// The package lives in the net/grpc module: it is the only part of the
// header helpers that depends on google.golang.org/grpc, which keeps the
// base net module free of it.
//
// Example:
//
//	ip := extractor.Extract(ctx, peerAddr, metadata.NewHeaderGetter(ctx))
package metadata
