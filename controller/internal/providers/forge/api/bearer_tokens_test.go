// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// makeToken builds a signed JWT carrying claims.
//
// The signing key is local and is not a credential, because nothing verifies
// these tokens. parseBearerTokenExpiration calls jwt.Parse with a NIL keyfunc,
// so the library decodes the claims and never checks the signature. The
// signature has no bearing on what this test measures, and any key gives the
// same result.
//
// The fixtures are built here rather than pasted in as literals. Three reasons,
// in order of importance:
//
//  1. A pasted JWT cannot be told apart from a real one by a secret scanner, by
//     nSpect, or by a reader. This file used to hold two REAL signed NGC bearer
//     tokens — iss=authn.nvidia.com, aud=ngc, a real signing-key id and intact
//     signatures — in a repository that is going public.
//  2. Claims written as a Go map say what each case tests. A base64 blob does
//     not, so nobody could tell which case covered what without decoding it.
//  3. Nothing here can rot into a confusing failure, because the parser reads
//     the claims this file writes.
func makeToken(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()

	const signingKey = "bearer-token-test-signing-key"

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(signingKey))
	if err != nil {
		t.Fatalf("building test token: %v", err)
	}
	return signed
}

// stripSignature removes the signature and keeps the trailing dot, giving the
// header.payload. shape. It is its own case because the parser must still read
// the claims from it.
func stripSignature(t *testing.T, token string) string {
	t.Helper()

	i := strings.LastIndex(token, ".")
	if i < 0 {
		t.Fatalf("token has no signature separator: %q", token)
	}
	return token[:i+1]
}

func TestParseBearerTokenExpiration(t *testing.T) {
	// Fixed instants rather than time.Now(), so a failure message is stable and
	// the assertion cannot race the clock. These are the same two values the
	// original fixtures carried, so the test still covers the same arithmetic.
	firstExp := time.Unix(1756495766, 0)
	secondExp := time.Unix(1756495796, 0)

	claims := func(exp *time.Time) jwt.MapClaims {
		c := jwt.MapClaims{
			"iss": "test-issuer",
			"aud": "test-audience",
			"sub": "test-subject",
		}
		if exp != nil {
			c["exp"] = exp.Unix()
		}
		return c
	}

	firstToken := makeToken(t, claims(&firstExp))
	secondToken := makeToken(t, claims(&secondExp))
	noExpToken := makeToken(t, claims(nil))

	tests := []struct {
		name    string
		token   string
		want    time.Time
		wantErr bool
	}{
		{
			name:    "valid token",
			token:   firstToken,
			want:    firstExp,
			wantErr: false,
		},
		{
			name:    "valid token, different expiration",
			token:   secondToken,
			want:    secondExp,
			wantErr: false,
		},
		{
			name:    "valid token no signature",
			token:   stripSignature(t, secondToken),
			want:    secondExp,
			wantErr: false,
		},
		{
			name:    "valid token no expiration",
			token:   noExpToken,
			want:    time.Time{},
			wantErr: true,
		},
		{
			name:    "invalid token",
			token:   "foobar",
			want:    time.Time{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBearerTokenExpiration(tt.token)

			if (err != nil) != tt.wantErr {
				t.Errorf("parseBearerTokenExpiration() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !got.Equal(tt.want) {
				t.Errorf("parseBearerTokenExpiration() = %v, want %v", got, tt.want)
			}
		})
	}
}
