// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package client

import (
	"io"
)

// CloseBody closes an HTTP response body. It is a no-op when body is nil and
// all errors are intentionally discarded — this helper is meant for defer
// statements in call sites that do not care about the cleanup result.
//
// It does not read the body: since Go 1.27 [http.Transport] drains an unread
// HTTP/1 body after Close in the background, within its own size and time
// bounds, so the connection is reused without blocking the caller.
//
// Typical usage:
//
//	resp, err := httpClient.Do(req)
//	if err != nil {
//	    return err
//	}
//	defer client.CloseBody(resp.Body)
func CloseBody(body io.ReadCloser) {
	if body == nil {
		return
	}
	_ = body.Close()
}
