// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package jwtbearer

import (
	"cmp"
	"context"
	"crypto"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/altessa-s/go-atlas-core/runtime/singleflight"
	"github.com/altessa-s/go-atlas-core/types/nilcheck"
	"github.com/altessa-s/go-atlas-core/types/redacted"

	"github.com/altessa-s/go-atlas-net/httpx"
	"github.com/altessa-s/go-atlas-net/urlx"
)

// GrantType identifies the RFC 7523 authorization grant.
const GrantType = "urn:ietf:params:oauth:grant-type:jwt-bearer"

// Grant errors contain no signing material or provider-supplied text.
var (
	ErrConfig   = errors.New("jwtbearer: invalid configuration")
	ErrSigning  = errors.New("jwtbearer: signing failed")
	ErrRequest  = errors.New("jwtbearer: token request failed")
	ErrResponse = errors.New("jwtbearer: invalid token response")
)

// Token is a value copy of an issued credential.
type Token struct {
	AccessToken     redacted.RedactedString
	RefreshToken    redacted.RedactedString
	IDToken         redacted.RedactedString
	TokenType       string
	Scope           string
	IssuedTokenType string
	Expiry          time.Time
}

type cached struct {
	token     Token
	refreshAt time.Time
}

// Source caches one credential's tokens. Create it with New.
type Source struct {
	endpoint string
	signer   crypto.Signer
	header   string
	claims   claims
	doer     httpx.Doer
	opts     *options
	flight   *singleflight.Group[struct{}, Token]
	current  atomic.Pointer[cached]
}

// New creates a source with an RSA signer and an injected HTTP client.
// Custom Doers must honor context and disable redirects. Use ParsePrivateKey for PEM.
func New(doer httpx.Doer, endpoint, issuer string, signer crypto.Signer, opts ...Option) (*Source, error) {
	o := newOptions(opts...)
	if nilcheck.IsNil(doer) || nilcheck.IsNil(signer) || nilcheck.IsNil(o.clock) || strings.TrimSpace(issuer) == "" {
		return nil, ErrConfig
	}

	pub, ok := signer.Public().(*rsa.PublicKey)
	if !ok || pub == nil || pub.N == nil {
		return nil, ErrConfig
	}

	if !validEndpoint(endpoint, o) {
		return nil, ErrConfig
	}

	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if o.keyID != "" {
		header["kid"] = o.keyID
	}

	encoded, err := json.Marshal(header)
	if err != nil {
		return nil, ErrConfig
	}

	return &Source{
		endpoint: endpoint,
		doer:     noRedirect(doer),
		signer:   signer,
		header:   base64.RawURLEncoding.EncodeToString(encoded),
		claims: claims{
			Issuer:    issuer,
			Audience:  cmp.Or(o.audience, endpoint),
			Subject:   o.subject,
			Principal: o.subject,
			Scope:     strings.Join(o.scopes, " "),
		},
		opts: o,
		flight: singleflight.New[struct{}, Token](
			singleflight.WithLifecycle(context.Background()),
			singleflight.WithTimeout(time.Duration(math.MaxInt64)),
		),
	}, nil
}

func validEndpoint(endpoint string, o *options) bool {
	schemes := []string{"https"}
	if o.insecureHTTP {
		schemes = append(schemes, "http")
	}

	_, err := urlx.ParseBaseURL(endpoint, urlx.WithAllowedSchemes(schemes...), urlx.WithEndpointMode())

	return err == nil
}

func noRedirect(doer httpx.Doer) httpx.Doer {
	if c, ok := doer.(*http.Client); ok {
		cp := *c
		cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

		return &cp
	}

	return doer
}

// Token returns a cached token or joins a refresh. Cancellation stops the shared
// POST when its last waiter leaves. A fresh cache hit requires no live context.
func (s *Source) Token(ctx context.Context) (Token, error) {
	if c := s.fresh(); c != nil {
		return c.token, nil
	}

	return s.flight.Do(ctx, struct{}{}, s.refresh)
}

func (s *Source) fresh() *cached {
	c := s.current.Load()
	if c != nil && s.opts.clock.Now().Before(c.refreshAt) {
		return c
	}

	return nil
}

func (s *Source) refresh(ctx context.Context) (Token, error) {
	previous := s.current.Load()

	now := s.opts.clock.Now()
	if previous != nil && now.Before(previous.refreshAt) {
		return previous.token, nil
	}

	tok, err := s.fetch(ctx, now)
	if err != nil {
		return Token{}, err
	}

	if err := ctx.Err(); err != nil {
		return Token{}, err
	}

	if tok.Expiry.IsZero() {
		return tok, nil
	}

	const marginDivisor = 2

	margin := min(s.opts.expiryMargin, tok.Expiry.Sub(now)/marginDivisor)
	// A detached refresh may finish after a newer flight has filled the cache.
	s.current.CompareAndSwap(previous, &cached{token: tok, refreshAt: tok.Expiry.Add(-margin)})

	return tok, nil
}

// Fetch signs and exchanges a fresh assertion directly on ctx, bypassing the cache.
func (s *Source) Fetch(ctx context.Context) (Token, error) {
	return s.fetch(ctx, s.opts.clock.Now())
}

func (s *Source) fetch(ctx context.Context, now time.Time) (Token, error) {
	if err := ctx.Err(); err != nil {
		return Token{}, err
	}

	assertion, err := s.assertion(now)
	if err != nil {
		return Token{}, err
	}

	return exchange(ctx, s.doer, s.endpoint, assertion, now, s.opts.optionalExpiry)
}
