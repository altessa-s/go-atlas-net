// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import (
	"net/http"

	coreio "github.com/altessa-s/go-atlas-core/io"
)

// newResponseSizeRoundTripper enforces the response size limit of
// [WithMaxResponseSize] on the replies of next; a non-positive limit returns
// next unchanged. A reply whose Content-Length exceeds limit is closed unread
// and fails with a [ResponseSizeError] that keeps its status and header for
// status-specific policies; a reply of unknown length is read through a
// limiting reader. It sits above the circuit breaker, so an oversized reply
// counts as a breaker success, and below the retry layer, which never
// retries the [ResponseSizeError].
func newResponseSizeRoundTripper(next http.RoundTripper, limit int64) http.RoundTripper {
	if limit <= 0 {
		return next
	}
	return idleClosingRoundTripper{next: next, RoundTripperFunc: func(req *http.Request) (*http.Response, error) {
		res, err := next.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		if res.ContentLength > limit {
			_ = res.Body.Close() //nolint:errcheck
			return nil, &ResponseSizeError{
				Limit:  limit,
				Size:   res.ContentLength,
				Status: res.StatusCode,
				Header: res.Header,
			}
		}
		if res.ContentLength < 0 {
			res.Body = coreio.NewLimitedReadCloser(res.Body, limit)
		}
		return res, nil
	}}
}
