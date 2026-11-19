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
// The request is handed to the next round tripper once; when it has a body,
// that attempt carries no GetBody, so net/http does not resend it either. A
// bodyless request carrying an Idempotency-Key or X-Idempotency-Key header
// may still be resent by net/http on a reused HTTP/1.1 connection (see
// [TransportDriver]).
func ContextWithoutRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, retryDisabledKey{}, true)
}

// RetryDisabled reports whether ctx was marked with [ContextWithoutRetry].
func RetryDisabled(ctx context.Context) bool {
	disabled, _ := ctx.Value(retryDisabledKey{}).(bool)

	return disabled
}
