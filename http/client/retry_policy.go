// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import (
	"context"
	"net/http"
	"slices"
)

// DefaultRetryableStatuses returns the reply statuses the built-in retry
// classification retries by default: 408 Request Timeout, 429 Too Many
// Requests, 502 Bad Gateway, 503 Service Unavailable and 504 Gateway Timeout
// (see [WithRetryableStatuses]). Each call returns a fresh slice.
func DefaultRetryableStatuses() []int {
	return []int{
		http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout,
	}
}

// defaultRetryableStatuses backs [DefaultRetryPolicy]; it is never modified.
var defaultRetryableStatuses = DefaultRetryableStatuses()

// DefaultRetryPolicy is the built-in retry classification in the
// [RetryPolicyHandler] form, for a custom policy that delegates to it for the
// cases it does not decide itself. It retries a transport error unless
// another attempt cannot help — the context ended, an SSRF refusal (an
// [SSRFError] or any error matching [ErrSSRFBlocked]), a limiter refusal
// (limiters.ErrRateLimited), an open circuit breaker, an oversized response
// ([ResponseSizeError]), an unsupported URL scheme, a rejected server
// certificate (an [x509.UnknownAuthorityError], [x509.HostnameError],
// [x509.CertificateInvalidError] or any [tls.CertificateVerificationError]),
// a refused connection, or too many redirects reported by a transport that
// follows redirects itself — and returns such a failure as the error. These
// errors are matched by type in the bare form a round tripper receives, not
// only inside a [*url.Error]. [net/http.Client] enforces its own redirect
// limit above the round tripper, so that failure is never retried. A reply with a status of [DefaultRetryableStatuses]
// is retried (its Retry-After is honored as configured); any other status
// of 400 and above, or 0, is returned as an [UnexpectedStatusError]; the
// remaining replies are accepted.
//
// It always uses [DefaultRetryableStatuses]: [WithRetryableStatuses] changes
// the statuses of the built-in classification, which applies when no
// [WithRetryPolicyHandler] is set.
//
// DefaultRetryPolicy classifies by status and error only, never by method:
// a policy built on it replays a POST like a GET. The built-in classification
// (no [WithRetryPolicyHandler]) adds the method check itself and replays only
// idempotent requests (GET, HEAD, OPTIONS, TRACE, PUT, DELETE) and requests
// with an Idempotency-Key or X-Idempotency-Key header. A custom policy that
// wants the same guard checks resp.Request.Method (see the second example).
//
// Example:
//
//	policy := func(ctx context.Context, resp *http.Response, err error) (bool, error) {
//	    if err == nil && resp.StatusCode == http.StatusConflict {
//	        return true, nil // also retry 409
//	    }
//	    return httpclient.DefaultRetryPolicy(ctx, resp, err)
//	}
//	c := httpclient.New(httpclient.WithRetryPolicyHandler(policy))
//
// Never replaying a POST on a retryable status (resp.Request is the attempt's
// request). A transport error comes without a response, so a policy cannot see
// the method there: a separate client built with [WithoutRetry] for
// non-idempotent requests avoids every replay.
//
//	noPostReplay := func(ctx context.Context, resp *http.Response, err error) (bool, error) {
//	    retry, perr := httpclient.DefaultRetryPolicy(ctx, resp, err)
//	    if retry && resp != nil && resp.Request != nil && resp.Request.Method == http.MethodPost {
//	        return false, &httpclient.UnexpectedStatusError{Status: resp.StatusCode, Method: http.MethodPost}
//	    }
//	    return retry, perr
//	}
func DefaultRetryPolicy(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if err != nil {
		classified := classifyTransportError(ctx, err)
		if retryable(classified) {
			return true, err
		}
		//nolint:errorlint // classifyTransportError returns the marker as the outermost layer
		if nr, ok := classified.(*NonRetryableError); ok {
			// The policy-handler protocol marks a (false, err) result
			// non-retryable itself.
			return false, nr.Err
		}
		return false, classified
	}
	if resp == nil {
		return false, nil
	}
	return classifyStatus(resp, defaultRetryableStatuses)
}

// classifyStatus decides on a reply: retry (true) for a status in retryable,
// an [UnexpectedStatusError] for any other status of 400 and above (or 0),
// and nil for the remaining replies.
func classifyStatus(resp *http.Response, retryable []int) (bool, error) {
	if slices.Contains(retryable, resp.StatusCode) {
		return true, nil
	}
	if resp.StatusCode == 0 || resp.StatusCode >= http.StatusBadRequest {
		return false, unexpectedStatus(resp)
	}
	return false, nil
}
