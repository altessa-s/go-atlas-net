// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package jwtbearer_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-core/time/clock"

	"github.com/altessa-s/go-atlas-net/httpx"
	"github.com/altessa-s/go-atlas-net/oauth2/jwtbearer"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

const endpoint = "https://issuer.test/token"

func keyFor(t testing.TB) *rsa.PrivateKey {
	t.Helper()

	k, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	return k
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
}

func TestSignedGrant(t *testing.T) {
	t.Parallel()
	key := keyFor(t)
	now := time.Unix(100, 0)
	cl := clock.NewFake(now)

	type contextKey struct{}

	ctx := context.WithValue(t.Context(), contextKey{}, "trace")
	calls := 0
	doer := httpx.DoerFunc(func(req *http.Request) (*http.Response, error) {
		calls++

		require.Equal(t, "trace", req.Context().Value(contextKey{}))
		require.Equal(t, "POST", req.Method)
		require.Equal(t, endpoint, req.URL.String())
		require.Equal(t, "application/x-www-form-urlencoded", req.Header.Get("Content-Type"))
		require.NoError(t, req.ParseForm())
		require.Equal(t, jwtbearer.GrantType, req.Form.Get("grant_type"))
		parts := strings.Split(req.Form.Get("assertion"), ".")
		require.Len(t, parts, 3)
		raw, err := base64.RawURLEncoding.DecodeString(parts[0])
		require.NoError(t, err)
		require.JSONEq(t, `{"alg":"RS256","typ":"JWT","kid":"key"}`, string(raw))
		raw, err = base64.RawURLEncoding.DecodeString(parts[1])
		require.NoError(t, err)

		var claims map[string]any
		require.NoError(t, json.Unmarshal(raw, &claims))
		require.Equal(t, map[string]any{"iss": "issuer", "aud": "aud", "scope": "a b", "sub": "admin", "prn": "admin", "iat": float64(cl.Now().Unix()), "exp": float64(cl.Now().Unix() + 3600)}, claims)

		sig, err := base64.RawURLEncoding.DecodeString(parts[2])
		require.NoError(t, err)

		hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		require.NoError(t, rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, hash[:], sig))

		return response(200, `{"access_token":"value","expires_in":10,"refresh_token":"refresh","token_type":"Bearer"}`), nil
	})
	s, err := jwtbearer.New(doer, endpoint, "issuer", key, jwtbearer.WithScopes("a", "b"), jwtbearer.WithAudience("aud"), jwtbearer.WithSubject("admin"), jwtbearer.WithKeyID("key"), jwtbearer.WithClock(cl))
	require.NoError(t, err)
	first, err := s.Token(ctx)
	require.NoError(t, err)
	require.Equal(t, "value", first.AccessToken.Expose())
	require.Equal(t, now.Add(10*time.Second), first.Expiry)
	cl.Advance(4 * time.Second)

	again, err := s.Token(ctx)
	require.NoError(t, err)
	require.Equal(t, first, again)
	require.Equal(t, 1, calls)
	cl.Advance(time.Second)

	_, err = s.Token(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
}

func TestParsePrivateKey(t *testing.T) {
	t.Parallel()
	k := keyFor(t)
	der, err := x509.MarshalPKCS8PrivateKey(k)
	require.NoError(t, err)

	for _, v := range [][]byte{x509.MarshalPKCS1PrivateKey(k), der} {
		parsed, err := jwtbearer.ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: v}))
		require.NoError(t, err)
		require.True(t, k.Equal(parsed))
	}

	for _, v := range [][]byte{[]byte("secret"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("secret")})} {
		_, err := jwtbearer.ParsePrivateKey(v)
		require.ErrorIs(t, err, jwtbearer.ErrConfig)
	}
}

func TestRefreshCancellation(t *testing.T) {
	t.Parallel()
	key := keyFor(t)
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32

		started := make(chan context.Context, 2)
		finish := make(chan struct{})
		doer := httpx.DoerFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)

			started <- r.Context()

			select {
			case <-finish:
				return response(200, `{"access_token":"ok","expires_in":1}`), nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		})
		s, err := jwtbearer.New(doer, endpoint, "issuer", key)
		require.NoError(t, err)

		run := func(ctx context.Context) <-chan error {
			ch := make(chan error, 1)

			go func() { _, err := s.Token(ctx); ch <- err }()

			return ch
		}
		ctx, cancel := context.WithCancel(t.Context())
		first := run(ctx)
		work := <-started
		ctx2, cancel2 := context.WithCancel(t.Context())
		second := run(ctx2)

		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-first, context.Canceled)
		require.NoError(t, work.Err())
		cancel2()
		require.ErrorIs(t, <-second, context.Canceled)
		synctest.Wait()
		require.ErrorIs(t, work.Err(), context.Canceled)
		require.EqualValues(t, 1, calls.Load())
		third := run(t.Context())

		<-started
		close(finish)
		require.NoError(t, <-third)
		require.EqualValues(t, 2, calls.Load())
	})
}

func TestExchangeFailures(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		status int
		body   string
		cause  error
	}{
		{"rejected", 403, "private", jwtbearer.ErrRequest},
		{"malformed", 200, "private", jwtbearer.ErrResponse},
		{"oversize", 200, strings.Repeat("x", jwtbearer.MaxResponseBytes+20), jwtbearer.ErrResponse},
		{"emptyToken", 200, `{"expires_in":20}`, jwtbearer.ErrResponse},
		{"missingExpiry", 200, `{"access_token":"private"}`, jwtbearer.ErrResponse},
		{"fractionAfterOverflow", 200, `{"access_token":"private","expires_in":99999999999999999999999.5}`, jwtbearer.ErrResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := httpx.DoerFunc(func(*http.Request) (*http.Response, error) { return response(tc.status, tc.body), nil })
			_, err := jwtbearer.Exchange(t.Context(), d, endpoint, "private")
			require.ErrorIs(t, err, tc.cause)

			var re *jwtbearer.ResponseError
			require.ErrorAs(t, err, &re)
			require.Equal(t, tc.status, re.Status)
			require.Equal(t, min(len(tc.body), jwtbearer.MaxResponseBytes+1), re.Bytes)
			require.NotContains(t, err.Error(), "private")
		})
	}

	d := httpx.DoerFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("private") })
	_, err := jwtbearer.Exchange(t.Context(), d, endpoint, "private")
	require.ErrorIs(t, err, jwtbearer.ErrRequest)
	require.NotContains(t, err.Error(), "private")
}

func TestEndpointAndSigner(t *testing.T) {
	t.Parallel()
	key := keyFor(t)

	client := &http.Client{Transport: httpx.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "issuer.test", req.URL.Host)

		r := response(307, "private")
		r.Header = http.Header{"Location": {"https://other.test/token"}}

		return r, nil
	})}
	for _, endpoint := range []string{"", "http://issuer.test/token", "invalid", "https://user@issuer.test", "https://issuer.test/path#secret", "https://issuer.test/path?q=1#", "https://:443/token?q=1", "https:///token", "ftp://issuer.test/token"} {
		_, err := jwtbearer.New(client, endpoint, "issuer", key)
		require.ErrorIs(t, err, jwtbearer.ErrConfig)
	}

	_, err := jwtbearer.New(client, endpoint, "", key)
	require.ErrorIs(t, err, jwtbearer.ErrConfig)
	_, err = jwtbearer.New(client, endpoint, "issuer", (*rsa.PrivateKey)(nil))
	require.ErrorIs(t, err, jwtbearer.ErrConfig)
	_, err = jwtbearer.New(client, endpoint, "issuer", &rsa.PrivateKey{})
	require.ErrorIs(t, err, jwtbearer.ErrConfig)
	_, err = jwtbearer.New(client, endpoint, "issuer", key, jwtbearer.WithClock(nil))
	require.ErrorIs(t, err, jwtbearer.ErrConfig)
	s, err := jwtbearer.New(client, "http://issuer.test/token", "issuer", key, jwtbearer.WithInsecureHTTP())
	require.NoError(t, err)
	_, err = s.Fetch(t.Context())

	var re *jwtbearer.ResponseError
	require.ErrorAs(t, err, &re)
	require.Equal(t, 307, re.Status)
	require.Nil(t, client.CheckRedirect)
}

func TestOptionalExpiry(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	d := httpx.DoerFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(200, `{"access_token":"access","token_type":"bearer","id_token":"id","scope":"read"}`), nil
	})
	s, err := jwtbearer.New(d, endpoint, "issuer", keyFor(t), jwtbearer.WithOptionalExpiry(), jwtbearer.WithExpiryMargin(0))
	require.NoError(t, err)

	for range 2 {
		token, err := s.Token(t.Context())
		require.NoError(t, err)
		require.True(t, token.Expiry.IsZero())
		require.Equal(t, "id", token.IDToken.Expose())
		require.Equal(t, "read", token.Scope)
	}

	require.EqualValues(t, 2, calls.Load())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = s.Fetch(ctx)
	require.ErrorIs(t, err, context.Canceled)
	_, err = jwtbearer.Exchange(ctx, nil, endpoint, "assertion")
	require.ErrorIs(t, err, jwtbearer.ErrConfig)
}

type brokenReader struct{ err error }

func (r brokenReader) Read([]byte) (int, error) { return 0, r.err }

func TestReadError(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		cause, want error
	}{
		{"failure", errors.New("private"), jwtbearer.ErrRequest},
		{"deadline", errors.Join(errors.New("private"), context.DeadlineExceeded), context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := httpx.DoerFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(brokenReader{tc.cause})}, nil
			})
			_, err := jwtbearer.Exchange(t.Context(), d, endpoint, "assertion")
			require.ErrorIs(t, err, tc.want)
			require.ErrorIs(t, err, tc.cause)
			require.ErrorIs(t, err, jwtbearer.ErrRequest)
			require.NotContains(t, err.Error(), "private")
		})
	}
}

func TestOpaqueClaimsAndEndpointQuery(t *testing.T) {
	t.Parallel()

	key := keyFor(t)
	for _, tc := range []struct{ name, value, query string }{
		{"padded", "  a/b  ", "?tenant=a%2Fb&tenant=c+d"},
		{"whitespace", " \t ", "?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			target := endpoint + tc.query
			d := httpx.DoerFunc(func(req *http.Request) (*http.Response, error) {
				require.Equal(t, target, req.URL.String())
				require.NoError(t, req.ParseForm())
				parts := strings.Split(req.PostForm.Get("assertion"), ".")
				require.Len(t, parts, 3)

				for i, want := range []map[string]string{{"kid": tc.value}, {"iss": tc.value, "sub": tc.value, "prn": tc.value, "aud": target}} {
					raw, err := base64.RawURLEncoding.DecodeString(parts[i])
					require.NoError(t, err)

					var fields map[string]any
					require.NoError(t, json.Unmarshal(raw, &fields))

					for k, v := range want {
						require.Equal(t, v, fields[k], k)
					}
				}

				return response(200, `{"access_token":"ok","expires_in":30}`), nil
			})
			s, err := jwtbearer.New(d, target, tc.value, key, jwtbearer.WithKeyID(&tc.value), jwtbearer.WithSubject(tc.value), jwtbearer.WithClock(clock.NewFake(time.Unix(0, 0))), jwtbearer.WithMaxLifetime(time.Second))
			require.NoError(t, err)
			tok, err := s.Fetch(t.Context())
			require.NoError(t, err)
			require.Equal(t, time.Unix(1, 0), tok.Expiry)
		})
	}
}

func TestLifetime(t *testing.T) {
	t.Parallel()

	now := time.Unix(100, 0)

	for _, tc := range []struct {
		name, seconds string
		cap, want     time.Duration
	}{
		{"short", "2", time.Minute, 2 * time.Second},
		{"capped", `"20"`, 1500 * time.Millisecond, 1500 * time.Millisecond},
		{"lastWholeSecond", "9223372036", 0, 9223372036 * time.Second},
		{"durationOverflow", "9223372037", 0, math.MaxInt64},
		{"integerOverflow", `"999999999999999999999999999"`, 0, math.MaxInt64},
		{"overflowCapped", "9223372036854775807", time.Hour, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := httpx.DoerFunc(func(*http.Request) (*http.Response, error) {
				return response(200, `{"extension":{"nested":[1,2]},"access_token":"ok","expires_in":`+tc.seconds+`}`), nil
			})
			tok, err := jwtbearer.Exchange(t.Context(), d, endpoint, "assertion", jwtbearer.WithClock(clock.NewFake(now)), jwtbearer.WithMaxLifetime(tc.cap))
			require.NoError(t, err)
			require.Equal(t, "ok", tok.AccessToken.Expose())
			require.Equal(t, now.Add(tc.want), tok.Expiry)
		})
	}
}

func TestResponseLimit(t *testing.T) {
	t.Parallel()

	const (
		valid    = `{"access_token":"ok","expires_in":1}`
		rejected = `{"error":"invalid_grant","error_description":"private","error_uri":"urn:private"}`
	)
	for _, tc := range []struct {
		name, body string
		status     int
		limit      int64
		cause      error
	}{
		{"exact", valid, 200, int64(len(valid)), nil},
		{"over", valid, 200, int64(len(valid) - 1), jwtbearer.ErrResponse},
		{"larger", valid + strings.Repeat(" ", jwtbearer.MaxResponseBytes), 200, 16 << 20, nil},
		{"rejected", rejected, 400, 1 << 16, jwtbearer.ErrRequest},
		{"truncated", rejected, 400, 4, jwtbearer.ErrRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := httpx.DoerFunc(func(*http.Request) (*http.Response, error) { return response(tc.status, tc.body), nil })

			_, err := jwtbearer.Exchange(t.Context(), d, endpoint, "private", jwtbearer.WithMaxResponseBytes(tc.limit))
			if tc.cause == nil {
				require.NoError(t, err)
				return
			}

			require.ErrorIs(t, err, tc.cause)

			var re *jwtbearer.ResponseError
			require.ErrorAs(t, err, &re)
			require.EqualValues(t, min(int64(len(tc.body)), tc.limit+1), re.Bytes)
			require.NotContains(t, err.Error(), "private")

			if tc.status == 400 {
				require.Equal(t, tc.body[:min(int64(len(tc.body)), tc.limit)], re.Body.Expose())

				if int64(len(tc.body)) <= tc.limit {
					require.Equal(t, "invalid_grant", re.ErrorCode.Expose())
					require.Equal(t, "private", re.ErrorDescription.Expose())
					require.Equal(t, "urn:private", re.ErrorURI.Expose())
				} else {
					require.Empty(t, re.ErrorCode.Expose())
				}
			}
		})
	}
}

func TestTransportErrorChain(t *testing.T) {
	t.Parallel()

	refused := errors.New("redirect refused: private")

	size := &httpclient.ResponseSizeError{Status: 403}
	for _, tc := range []struct {
		name  string
		cause error
	}{
		{"redirect", refused}, {"size", size}, {"canceled", context.Canceled}, {"deadline", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			transport := &url.Error{Op: "POST", URL: "https://private", Err: tc.cause}
			d := httpx.DoerFunc(func(*http.Request) (*http.Response, error) { cancel(); return nil, transport })
			_, err := jwtbearer.Exchange(ctx, d, endpoint, "private")
			require.ErrorIs(t, err, jwtbearer.ErrRequest)
			require.ErrorIs(t, err, tc.cause)
			require.ErrorIs(t, err, context.Canceled)
			got, ok := errors.AsType[*url.Error](err)
			require.True(t, ok)
			require.Same(t, transport, got)

			if tc.cause == size {
				require.Same(t, size, httpclient.IsResponseSizeError(err))
			}

			require.NotContains(t, err.Error(), "private")
		})
	}
}
