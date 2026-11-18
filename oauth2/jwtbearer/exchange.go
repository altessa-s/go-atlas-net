// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package jwtbearer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/altessa-s/go-atlas-core/types/nilcheck"
	"github.com/altessa-s/go-atlas-core/types/redacted"

	"github.com/altessa-s/go-atlas-net/httpx"
)

// MaxResponseBytes bounds successful and unsuccessful response bodies.
const MaxResponseBytes = 1 << 20

// ResponseError describes a rejected response. Bytes is the observed body
// length, capped at MaxResponseBytes+1. Unwrap identifies the failure class.
type ResponseError struct {
	Status int
	Bytes  int
	cause  error
}

// Error returns a secret-free diagnostic.
func (e *ResponseError) Error() string {
	return fmt.Sprintf("%v (HTTP %d, %d bytes)", e.cause, e.Status, e.Bytes)
}

// Unwrap returns ErrRequest for a rejected request or read failure, or ErrResponse for an invalid successful response.
func (e *ResponseError) Unwrap() error { return e.cause }

// Exchange posts an externally signed assertion directly on ctx. This supports
// callers with existing signing algorithms or retry policies. WithClock and
// WithInsecureHTTP apply. Custom Doers must honor context and disable redirects.
func Exchange(ctx context.Context, doer httpx.Doer, endpoint, assertion string, opts ...Option) (Token, error) {
	o := newOptions(opts...)
	if nilcheck.IsNil(doer) || nilcheck.IsNil(o.clock) || assertion == "" || !validEndpoint(endpoint, o) {
		return Token{}, ErrConfig
	}

	return exchange(ctx, noRedirect(doer), endpoint, assertion, o.clock.Now(), o.optionalExpiry)
}

func exchange(ctx context.Context, doer httpx.Doer, endpoint, assertion string, now time.Time, optionalExpiry bool) (Token, error) {
	form := url.Values{"grant_type": {GrantType}, "assertion": {assertion}}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, ErrConfig
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := doer.Do(req)
	if err != nil {
		return Token{}, requestError(ctx, err)
	}

	if resp == nil || resp.Body == nil {
		return Token{}, ErrRequest
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))

	failure := &ResponseError{Status: resp.StatusCode, Bytes: len(data), cause: ErrRequest}
	if err != nil {
		if cause := requestError(ctx, err); cause != ErrRequest {
			return Token{}, cause
		}

		return Token{}, failure
	}

	if !httpx.IsSuccess(resp.StatusCode) {
		return Token{}, failure
	}

	failure.cause = ErrResponse
	if len(data) > MaxResponseBytes {
		return Token{}, failure
	}

	tok, err := parseResponse(data, now, optionalExpiry)
	if err != nil {
		return Token{}, failure
	}

	return tok, nil
}

func requestError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, cause) {
			return cause
		}
	}

	return ErrRequest
}

func parseResponse(data []byte, now time.Time, optionalExpiry bool) (Token, error) {
	var v struct {
		Access     string      `json:"access_token"`
		Refresh    string      `json:"refresh_token"`
		IDToken    string      `json:"id_token"`
		Type       string      `json:"token_type"`
		Scope      string      `json:"scope"`
		IssuedType string      `json:"issued_token_type"`
		Seconds    json.Number `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &v); err != nil || v.Access == "" {
		return Token{}, ErrResponse
	}

	var expiry time.Time

	if !optionalExpiry || (v.Seconds != "" && v.Seconds != "0") {
		secs, err := v.Seconds.Int64()
		if err != nil || secs <= 0 || secs > math.MaxInt64/int64(time.Second) {
			return Token{}, ErrResponse
		}

		expiry = now.Add(time.Duration(secs) * time.Second)
	}

	return Token{
		AccessToken:     redacted.RedactedString(v.Access),
		RefreshToken:    redacted.RedactedString(v.Refresh),
		IDToken:         redacted.RedactedString(v.IDToken),
		TokenType:       v.Type,
		Scope:           v.Scope,
		IssuedTokenType: v.IssuedType,
		Expiry:          expiry,
	}, nil
}
