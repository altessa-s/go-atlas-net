// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package httpx

import "net/http"

// Doer sends an HTTP request and returns the response. [*http.Client]
// satisfies it, which lets code that only sends requests accept a client, a
// decorated client or a test double behind one small interface.
//
// Implementations follow the [http.Client.Do] contract: on a nil error the
// caller owns the response body and must close it.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// DoerFunc is an adapter that allows ordinary functions to satisfy [Doer]. If
// f is a function with the appropriate signature, DoerFunc(f) is a [Doer]
// whose [DoerFunc.Do] method calls f.
//
// DoerFunc carries no state of its own; it is safe for concurrent use when the
// wrapped function is.
type DoerFunc func(req *http.Request) (*http.Response, error)

// Do calls f(req) and returns its results unchanged.
func (f DoerFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}
