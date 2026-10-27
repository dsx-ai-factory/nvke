// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnit_TransportVerifierValidatesOptionalNotBefore(t *testing.T) {
	t.Parallel()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	cfg := validTransportVerificationConfig()
	verifier, err := newVerifier(cfg, map[string]KeySource{
		NodeAgentIssuerName: staticPublicKey{key: &key.PublicKey},
	}, false)
	require.NoError(t, err)

	now := time.Now()

	type parameters struct {
		notBefore  *time.Time
		wantReason FailureReason
	}
	tests := map[string]parameters{
		"absent is accepted": {},
		"future value is rejected": {
			notBefore:  func() *time.Time { value := now.Add(time.Hour); return &value }(),
			wantReason: ReasonNotYetValid,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			claims := jwt.MapClaims{
				"iss": cfg.Issuers[0].Issuer,
				"sub": "spiffe://identity.example.test/bm/node-id",
				"aud": NodeAgentAudience,
				"exp": now.Add(time.Minute).Unix(),
			}
			if params.notBefore != nil {
				claims["nbf"] = params.notBefore.Unix()
			}
			token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
			token.Header["kid"] = "key-1"
			signed, signErr := token.SignedString(key)
			require.NoError(t, signErr)

			_, verifyErr := verifier.Verify(context.Background(), signed)
			if params.wantReason == "" {
				require.NoError(t, verifyErr)
				return
			}
			require.Error(t, verifyErr)
			reason, ok := ReasonOf(verifyErr)
			require.True(t, ok)
			assert.Equal(t, params.wantReason, reason)
		})
	}
}

func TestNodeAgentAudiencesAcceptsBothSpellings(t *testing.T) {
	t.Parallel()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	cfg := validTransportVerificationConfig()
	verifier, err := newVerifier(cfg, map[string]KeySource{
		NodeAgentIssuerName: staticPublicKey{key: &key.PublicKey},
	}, false)
	require.NoError(t, err)

	for aud, wantErr := range map[string]bool{
		NodeAgentAudience:       false,
		NodeAgentAudienceLegacy: false,
		"someone-else":          true,
	} {
		t.Run(aud, func(t *testing.T) {
			t.Parallel()
			token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
				"iss": cfg.Issuers[0].Issuer,
				"sub": "spiffe://identity.example.test/bm/node-id",
				"aud": aud,
				"exp": time.Now().Add(time.Minute).Unix(),
			})
			token.Header["kid"] = "key-1"
			signed, signErr := token.SignedString(key)
			require.NoError(t, signErr)

			_, verifyErr := verifier.Verify(context.Background(), signed)
			if wantErr {
				require.Error(t, verifyErr)
				return
			}
			require.NoError(t, verifyErr)
		})
	}
}

type staticPublicKey struct{ key crypto.PublicKey }

func (s staticPublicKey) KeyFor(context.Context, string) (crypto.PublicKey, error) { return s.key, nil }

func validTransportVerificationConfig() *Config {
	return &Config{
		Audiences: NodeAgentAudiences(),
		Issuers: []Issuer{{
			Name:       NodeAgentIssuerName,
			Issuer:     "https://identity.example.test",
			JWKS:       &JWKSKeys{URI: "https://identity.example.test/jwks"},
			Algorithms: []string{"RS256"},
		}},
	}
}
