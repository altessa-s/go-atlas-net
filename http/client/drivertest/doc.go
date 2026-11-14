// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

// Package drivertest provides local-server conformance tests for transport
// drivers. Run it from each driver's own test package. Passing is regression
// evidence, not containment: review every socket-opening path in the driver and
// its dependencies to ensure it uses DriverEnv.Dial.
//
// Requests that may have been processed must not be replayed, except that
// net/http may resend replayable requests on reused HTTP/1.1 connections after
// certain connection failures, even after writing them. Replayable means no
// body (nil or http.NoBody) or GetBody, and GET, HEAD, OPTIONS, TRACE or an
// Idempotency-Key or X-Idempotency-Key header. This transport behavior has no
// disable option and occurs below the client retry layer, even with WithoutRetry.
// HTTP/1.1 cases warm one or several idle connections; replayable requests
// can fail on successive reused connections, so there is no general two-send
// limit. The several-connection fixture drops requests even on fresh connections
// and requires an error before the deadline and no transmissions after return.
// Ordinary POST/PATCH without either header must not be replayed after possible
// processing; net/http may retry them if nothing was written and they have no
// body or GetBody. Consumed non-rewindable bodies must not be replayed.
// On HTTP/2, only protocol-proven unprocessed requests with replayable bodies
// may be resent. Run checks these rules with plain clients and WithoutRetry.
package drivertest
