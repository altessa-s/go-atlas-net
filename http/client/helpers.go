// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import (
	"io"
)

// CloseBody drains up to [maxDiscardDrain] bytes of an HTTP response body
// and closes it, so the next request reuses the connection right away; a
// larger body is abandoned together with its connection. It is a no-op when
// body is nil and all errors are intentionally discarded — this helper is
// meant for defer statements in call sites that do not care about the
// cleanup result.
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
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxDiscardDrain))
	_ = body.Close()
}
