// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package jwtbearer

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"time"
)

// ParsePrivateKey decodes an unencrypted PKCS#1 or PKCS#8 RSA private key.
func ParsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, ErrConfig
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	value, err := x509.ParsePKCS8PrivateKey(block.Bytes)

	key, ok := value.(*rsa.PrivateKey)
	if err != nil || !ok {
		return nil, ErrConfig
	}

	return key, nil
}

type claims struct {
	Issuer    string `json:"iss"`
	Audience  string `json:"aud"`
	Scope     string `json:"scope,omitempty"`
	Principal string `json:"prn,omitempty"`
	Subject   string `json:"sub,omitempty"`
	Issued    int64  `json:"iat"`
	Expires   int64  `json:"exp"`
}

func (s *Source) assertion(now time.Time) (string, error) {
	payload := s.claims
	payload.Issued = now.Unix()
	payload.Expires = now.Add(time.Hour).Unix()

	data, err := json.Marshal(payload)
	if err != nil {
		return "", ErrSigning
	}

	unsigned := s.header + "." + base64.RawURLEncoding.EncodeToString(data)
	digest := sha256.Sum256([]byte(unsigned))

	signature, err := s.signer.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		return "", ErrSigning
	}

	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
