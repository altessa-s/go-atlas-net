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
	"strconv"
	"strings"
	"time"

	"github.com/altessa-s/go-atlas-core/types/nilcheck"
	"github.com/altessa-s/go-atlas-core/types/redacted"

	"github.com/altessa-s/go-atlas-net/httpx"
)

// MaxResponseBytes is the default limit for successful and unsuccessful response bodies.
const MaxResponseBytes = 1 << 20

// ResponseError describes a rejected response. Bytes is the observed body
// length, capped at the configured limit plus one. Unwrap identifies the failure class.
type ResponseError struct {
	Status int
	Bytes  int
	// Body is the bounded body of a rejected HTTP response. The OAuth fields
	// are populated when the complete body contains an RFC 6749 error object.
	Body             redacted.RedactedString
	ErrorCode        redacted.RedactedString
	ErrorDescription redacted.RedactedString
	ErrorURI         redacted.RedactedString
	cause            error
}

// Error returns a secret-free diagnostic.
func (e *ResponseError) Error() string {
	return fmt.Sprintf("%v (HTTP %d, %d bytes)", e.cause, e.Status, e.Bytes)
}

// Unwrap returns ErrRequest for a rejected request or read failure, or ErrResponse for an invalid successful response.
func (e *ResponseError) Unwrap() error { return e.cause }

// Exchange posts an externally signed assertion directly on ctx. This supports
// callers with existing signing algorithms or retry policies. Endpoint, clock, response size and expiry options apply.
// Custom Doers must honor context and disable redirects.
func Exchange(ctx context.Context, doer httpx.Doer, endpoint, assertion string, opts ...Option) (Token, error) {
	o := newOptions(opts...)
	if nilcheck.IsNil(doer) || nilcheck.IsNil(o.clock) || assertion == "" || !validEndpoint(endpoint, o) {
		return Token{}, ErrConfig
	}

	return exchange(ctx, noRedirect(doer), endpoint, assertion, o.clock.Now(), o)
}

func exchange(ctx context.Context, doer httpx.Doer, endpoint, assertion string, now time.Time, o *options) (Token, error) {
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

	data, err := io.ReadAll(io.LimitReader(resp.Body, min(o.maxResponseBytes, math.MaxInt64-1)+1))

	failure := &ResponseError{Status: resp.StatusCode, Bytes: len(data), cause: ErrRequest}
	if err != nil {
		failure.cause = requestError(ctx, err)

		return Token{}, failure
	}

	if !httpx.IsSuccess(resp.StatusCode) {
		failure.setOAuthError(data, o.maxResponseBytes)
		return Token{}, failure
	}

	failure.cause = ErrResponse
	if int64(len(data)) > o.maxResponseBytes {
		return Token{}, failure
	}

	tok, err := parseResponse(data, now, o)
	if err != nil {
		return Token{}, failure
	}

	return tok, nil
}

func (e *ResponseError) setOAuthError(data []byte, limit int64) {
	e.Body = redacted.RedactedString(data[:min(int64(len(data)), limit)])
	if int64(len(data)) <= limit {
		var fields struct {
			Code        string `json:"error"`
			Description string `json:"error_description"`
			URI         string `json:"error_uri"`
		}
		if json.Unmarshal(data, &fields) == nil {
			e.ErrorCode = redacted.RedactedString(fields.Code)
			e.ErrorDescription = redacted.RedactedString(fields.Description)
			e.ErrorURI = redacted.RedactedString(fields.URI)
		}
	}
}

// transportError exposes transport and context causes only through unwrapping.
type transportError struct{ cause error }

func (e *transportError) Error() string        { return ErrRequest.Error() }
func (e *transportError) Unwrap() error        { return e.cause }
func (e *transportError) Is(target error) bool { return target == ErrRequest }

func requestError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		err = errors.Join(err, ctxErr)
	}

	return &transportError{cause: err}
}

func parseResponse(data []byte, now time.Time, o *options) (Token, error) {
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

	expiry, err := o.tokenExpiry(v.Seconds, now)
	if err != nil {
		return Token{}, err
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

func (o *options) tokenExpiry(seconds json.Number, now time.Time) (time.Time, error) {
	if o.optionalExpiry && (seconds == "" || seconds == "0") {
		return time.Time{}, nil
	}
	// json.Number already validated the JSON syntax. Exclude signed, fractional
	// and exponential forms before treating an integer range error as saturation.
	if strings.ContainsAny(seconds.String(), "-.eE") {
		return time.Time{}, ErrResponse
	}

	secs, err := seconds.Int64()
	if secs <= 0 || (err != nil && !errors.Is(err, strconv.ErrRange)) {
		return time.Time{}, ErrResponse
	}

	lifetime := time.Duration(math.MaxInt64)
	if secs <= math.MaxInt64/int64(time.Second) {
		lifetime = time.Duration(secs) * time.Second
	}

	if o.maxLifetime > 0 {
		lifetime = min(lifetime, o.maxLifetime)
	}

	return now.Add(lifetime), nil
}
