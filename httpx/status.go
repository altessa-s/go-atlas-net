// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package httpx

import "net/http"

// IsSuccess reports whether status is in the 2xx class (200–299).
func IsSuccess(status int) bool {
	return status >= http.StatusOK && status < http.StatusMultipleChoices
}

// IsRedirect reports whether status is in the 3xx class (300–399).
//
// The class check does not imply that a Location header is present: 304 Not
// Modified, for example, is in the class but is not followed.
func IsRedirect(status int) bool {
	return status >= http.StatusMultipleChoices && status < http.StatusBadRequest
}
