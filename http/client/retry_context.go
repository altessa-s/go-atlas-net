// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import "context"

// retryDisabledKey marks a context whose requests the retry layer attempts once.
type retryDisabledKey struct{}

// ContextWithoutRetry returns a copy of ctx under which the client retry loop
// makes at most one call to the next round tripper per request (none when the
// request is refused or its context ends first), whatever its method,
// idempotency key or the client's retry options and [RetryPolicyHandler]. The
// outcome is classified as for any request that is not replayed (a retryable
// status yields a [RetryExhaustedError] after the one attempt; a policy still
// classifies it). An eligible request still gets a replayable body (buffered
// when it has no GetBody): the mark caps the calls, not the body preparation.
// The exception is a client with no retries configured (WithRetryMax(0),
// WithoutRetry), which never buffers and hands the caller's body on as is.
//
// The scope is the client retry loop only; the transport below it keeps its
// own retries, of two kinds (see [TransportDriver]):
//
//   - after a possible processing: on a reused HTTP/1.1 connection net/http
//     may resend a request it already wrote if it is idempotent by method
//     (GET, HEAD, OPTIONS, TRACE) or carries an idempotency key, and has no
//     body or a GetBody. The method or the single key makes the repeat safe
//     (an ambiguous key is refused with [ErrAmbiguousIdempotencyKey]); an
//     unkeyed POST or PATCH is never resent once it may have been processed.
//   - when no processing occurred: net/http may retry any request, unkeyed
//     ones included, when nothing was written on a reused HTTP/1.1
//     connection (with no body or a GetBody), or when HTTP/2 proves the
//     stream unprocessed (REFUSED_STREAM, a GOAWAY below its stream ID). The
//     server never saw the earlier transmission.
func ContextWithoutRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, retryDisabledKey{}, true)
}

// RetryDisabled reports whether ctx was marked with [ContextWithoutRetry].
func RetryDisabled(ctx context.Context) bool {
	disabled, _ := ctx.Value(retryDisabledKey{}).(bool)

	return disabled
}
