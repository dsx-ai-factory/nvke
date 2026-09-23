// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth_test

// Synthetic tokens for the whole auth package. No real issuer will mint a token
// with a missing "exp" or a signature from the wrong key, so the negatives have
// to be minted here.

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/auth"
)

// errUnknownKeyID is deliberately an ordinary error, not auth.ErrKeysStale: an
// unknown kid means the token is wrong, not that CWE's key set is.
var errUnknownKeyID = errors.New("static key source: unknown key id")

const (
	// The SPIRE issuer below overrides the deployment audience and the Kubernetes
	// one does not, which is the audience-override matrix under test.
	deploymentAudience = "https://cwe.example.com"

	spireAudience = "spiffe://example.org/nke-service/cloud-workflow-engine"

	admittedNamespace = "nke-clients"
	admittedName      = "runner"
	admittedSubject   = "system:serviceaccount:nke-clients:runner"
	spireSubject      = "spiffe://example.org/nke-service/cloud-workflow-engine"

	// hostileIssuer is an "iss" no operator configured, shaped to look like one.
	hostileIssuer = "https://kubernetes.default.svc.cluster.local.attacker.test"

	// hugeBytes sizes the oversized values the log-amplification tests send:
	// generous next to any message this package builds, small next to net/http's
	// default MaxHeaderBytes.
	hugeBytes = 100 * 1024
)

// At returns a pointer to t. A nil TokenSpec time field omits the claim.
func At(t time.Time) *time.Time { return &t }

type SigningKey struct {
	KeyID   string
	Method  jwt.SigningMethod
	Private crypto.PrivateKey
	Public  crypto.PublicKey
}

// Key generation is the slowest thing in this package's tests, so keys are
// generated once per binary and shared. They are immutable once generated.
var (
	signingKeysMu sync.Mutex
	signingKeys   = map[string]SigningKey{}
)

func RSAKey(t *testing.T, kid string) SigningKey {
	t.Helper()
	return cachedKey(t, "RS256/"+kid, func() (SigningKey, error) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return SigningKey{}, err
		}
		return SigningKey{
			KeyID:   kid,
			Method:  jwt.SigningMethodRS256,
			Private: key,
			Public:  key.Public(),
		}, nil
	})
}

func ECDSAKey(t *testing.T, kid string) SigningKey {
	t.Helper()
	return cachedKey(t, "ES256/"+kid, func() (SigningKey, error) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return SigningKey{}, err
		}
		return SigningKey{
			KeyID:   kid,
			Method:  jwt.SigningMethodES256,
			Private: key,
			Public:  key.Public(),
		}, nil
	})
}

func cachedKey(t *testing.T, cacheKey string, generate func() (SigningKey, error)) SigningKey {
	t.Helper()
	signingKeysMu.Lock()
	defer signingKeysMu.Unlock()
	if key, ok := signingKeys[cacheKey]; ok {
		return key
	}
	key, err := generate()
	require.NoError(t, err, "generate %s", cacheKey)
	signingKeys[cacheKey] = key
	return key
}

// TokenSpec describes a synthetic token. Every claim that can be absent is
// nil-able, so "the token has no exp" is expressible rather than approximated.
type TokenSpec struct {
	Key SigningKey
	// Method and KeyID override Key's, for signing with a disallowed algorithm
	// or pointing a token at a key it was not signed with.
	Method jwt.SigningMethod
	KeyID  string
	// OmitKeyID drops the "kid" header, which nothing in the format requires.
	OmitKeyID bool

	// An empty Subject omits "sub".
	Issuer  string
	Subject string

	// Audience is written to "aud" verbatim: a string for the bare-string form,
	// []any for the array form Kubernetes and SPIRE emit, nil to omit it.
	Audience any

	// Written as numeric dates. A nil pointer omits the claim.
	ExpiresAt *time.Time
	NotBefore *time.Time
	IssuedAt  *time.Time

	// Extra is merged last and wins, so a test can write a malformed claim.
	Extra map[string]any
}

func MintToken(t *testing.T, spec TokenSpec) string {
	t.Helper()

	claims := jwt.MapClaims{}
	if spec.Issuer != "" {
		claims["iss"] = spec.Issuer
	}
	if spec.Subject != "" {
		claims["sub"] = spec.Subject
	}
	if spec.Audience != nil {
		claims["aud"] = spec.Audience
	}
	if spec.ExpiresAt != nil {
		claims["exp"] = jwt.NewNumericDate(*spec.ExpiresAt)
	}
	if spec.NotBefore != nil {
		claims["nbf"] = jwt.NewNumericDate(*spec.NotBefore)
	}
	if spec.IssuedAt != nil {
		claims["iat"] = jwt.NewNumericDate(*spec.IssuedAt)
	}
	for k, v := range spec.Extra {
		claims[k] = v
	}

	method := spec.Method
	if method == nil {
		method = spec.Key.Method
	}
	require.NotNil(t, method, "TokenSpec needs a signing method")

	token := jwt.NewWithClaims(method, claims)
	if !spec.OmitKeyID {
		kid := spec.KeyID
		if kid == "" {
			kid = spec.Key.KeyID
		}
		token.Header["kid"] = kid
	}

	signed, err := token.SignedString(spec.Key.Private)
	require.NoError(t, err, "sign synthetic token")
	return signed
}

// testConfig is built in Go rather than YAML; config_test.go covers the YAML
// path.
func testConfig() *auth.Config {
	return &auth.Config{
		Audiences: []string{deploymentAudience},
		Issuers: []auth.Issuer{
			{
				Name:       k8sName,
				Issuer:     k8sIssuer,
				JWKS:       &auth.JWKSKeys{URI: "https://kubernetes.default.svc/openid/v1/jwks"},
				Algorithms: []string{"RS256"},
				Admit: &auth.Admit{
					ServiceAccounts: []auth.ServiceAccountRule{{Namespace: admittedNamespace}},
				},
			},
			{
				Name:       spireName,
				Issuer:     spireIssuer,
				Audiences:  []string{spireAudience},
				JWKS:       &auth.JWKSKeys{URI: "https://spire.example.com/keys"},
				Algorithms: []string{"RS256", "ES256"},
				Admit: &auth.Admit{
					SPIFFE: &auth.SPIFFERule{
						TrustDomain:  "example.org",
						PathPrefixes: []string{"/nke-service/"},
					},
				},
			},
		},
	}
}

// fixture is a Verifier wired to in-process keys, plus the keys, so a case can
// sign with the wrong one.
type fixture struct {
	verifier *auth.Verifier
	// signing is the key both issuers' sources serve.
	signing SigningKey
	// elliptic is served too, so an ES256 token reaches the algorithm check
	// rather than failing on a missing key.
	elliptic SigningKey
	// rogue is never served by any source.
	rogue SigningKey
	// k8sKeys is exposed so a case can make it fail with ErrKeysStale.
	k8sKeys *StaticKeySource
}

func newFixture(t *testing.T) fixture {
	t.Helper()

	signing := RSAKey(t, "signing-1")
	elliptic := ECDSAKey(t, "elliptic-1")
	rogue := RSAKey(t, "rogue-1")

	k8sKeys := NewStaticKeySource(signing, elliptic)
	spireKeys := NewStaticKeySource(signing, elliptic)

	verifier, err := auth.NewVerifier(testConfig(), map[string]auth.KeySource{
		k8sName:   k8sKeys,
		spireName: spireKeys,
	})
	require.NoError(t, err)

	return fixture{
		verifier: verifier,
		signing:  signing,
		elliptic: elliptic,
		rogue:    rogue,
		k8sKeys:  k8sKeys,
	}
}

// The shape a bound service account token really has: nbf == iat, "aud" an
// array, and a "sub" that agrees with the nested "kubernetes.io" claim.
func validKubernetesToken(f fixture, now time.Time) TokenSpec {
	return TokenSpec{
		Key:       f.signing,
		Issuer:    k8sIssuer,
		Subject:   admittedSubject,
		Audience:  []any{deploymentAudience},
		IssuedAt:  At(now),
		NotBefore: At(now),
		ExpiresAt: At(now.Add(time.Hour)),
		Extra:     ServiceAccountClaims(admittedNamespace, admittedName),
	}
}

// The shape a JWT-SVID really has: no "nbf", "sub" the full SPIFFE ID, and the
// issuer's own audience.
func validSPIREToken(f fixture, now time.Time) TokenSpec {
	return TokenSpec{
		Key:       f.signing,
		Issuer:    spireIssuer,
		Subject:   spireSubject,
		Audience:  []any{spireAudience},
		IssuedAt:  At(now),
		ExpiresAt: At(now.Add(time.Hour)),
	}
}

// The ways a Kubernetes token is wrong, one spoiler each.
func spoilAudience(_ fixture, spec *TokenSpec) {
	spec.Audience = []any{"https://nke.wrong.invalid"}
}

// spoilExpiry backdates the whole token so nothing about it is borderline.
func spoilExpiry(_ fixture, spec *TokenSpec) {
	past := time.Now().Add(-24 * time.Hour)
	spec.IssuedAt, spec.NotBefore = At(past), At(past)
	spec.ExpiresAt = At(past.Add(time.Hour))
}

// spoilSignature signs with a key no source serves but points "kid" at one they
// all do, so the failure lands on the signature check and not on key lookup.
func spoilSignature(f fixture, spec *TokenSpec) {
	spec.Key = f.rogue
	spec.KeyID = f.signing.KeyID
}

func spoilIssuer(_ fixture, spec *TokenSpec) {
	spec.Issuer = hostileIssuer
}

// StaticKeySource satisfies auth.KeySource from a map of key ids.
type StaticKeySource struct {
	// Keys is keyed by "kid"; a token with no "kid" looks up "".
	Keys map[string]crypto.PublicKey
	// Err, when set, is returned instead of any key. auth.ErrKeysStale exercises
	// the stale-key path without a clock.
	Err error
}

func NewStaticKeySource(keys ...SigningKey) *StaticKeySource {
	byKeyID := make(map[string]crypto.PublicKey, len(keys))
	for _, k := range keys {
		byKeyID[k.KeyID] = k.Public
	}
	return &StaticKeySource{Keys: byKeyID, Err: nil}
}

func (s *StaticKeySource) KeyFor(_ context.Context, kid string) (crypto.PublicKey, error) {
	if s.Err != nil {
		return nil, s.Err
	}
	key, ok := s.Keys[kid]
	if !ok {
		return nil, errUnknownKeyID
	}
	return key, nil
}

var _ auth.KeySource = (*StaticKeySource)(nil)
