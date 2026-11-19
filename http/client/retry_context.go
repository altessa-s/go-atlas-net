// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import "context"

// retryDisabledKey marks a context whose requests the retry layer attempts once.
type retryDisabledKey struct{}

// ContextWithoutRetry returns a copy of ctx under which the client retry layer
// makes a single attempt per request, whatever its method, idempotency key or
// the client's retry options and [RetryPolicyHandler]. The outcome is
// classified as for any request that is not replayed (a retryable status
// yields a [RetryExhaustedError] after the one attempt; a policy still
// classifies it). Use it for a request that must never be resent by the
// client, such as a non-idempotent call whose key the caller cannot vouch for.
// The scope is the client retry loop: exactly one call to the next round
// tripper, with the request unmodified. net/http's own connection-level
// replay is not affected: on a reused HTTP/1.1 connection it may resend an
// idempotent request (GET, HEAD, OPTIONS, TRACE) or one carrying an
// idempotency key, and on HTTP/2 it retries a request refused with
// REFUSED_STREAM or a GOAWAY below its stream ID (see [TransportDriver]).
// That is safe: those requests are idempotent by method or by their single
// key (an ambiguous key is refused with [ErrAmbiguousIdempotencyKey]), the
// HTTP/2 ones were proven unprocessed, and an unkeyed POST or PATCH is never
// resent by net/http after it may have been processed.
func ContextWithoutRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, retryDisabledKey{}, true)
}

// RetryDisabled reports whether ctx was marked with [ContextWithoutRetry].
func RetryDisabled(ctx context.Context) bool {
	disabled, _ := ctx.Value(retryDisabledKey{}).(bool)

	return disabled
}
