// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/auth"
)

// verifyCase is one token and the answer it must get. Tokens are built valid and
// then spoiled: almost every case differs from a working one by a single field.
type verifyCase struct {
	name string
	// base builds the starting token; nil means a valid Kubernetes one.
	base func(f fixture, now time.Time) TokenSpec
	// spoil makes it wrong; nil leaves the token valid.
	spoil func(f fixture, spec *TokenSpec)
	// raw hands Verify a credential that was never a minted token.
	raw *string
	// prepare breaks the fixture rather than the token, for the controller's own failures.
	prepare func(f fixture)

	// wantReason is empty when verification must succeed.
	wantReason auth.FailureReason
	// wantIssuer is the configured issuer name the failure must carry. Empty means
	// the failure was raised before any entry was matched.
	wantIssuer string
	wantErrIs  error
	check      func(t *testing.T, p *auth.Principal)
}

func rawCredential(s string) *string { return &s }

func runVerifyCases(t *testing.T, cases []verifyCase) {
	t.Helper()

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)
			if tt.prepare != nil {
				tt.prepare(f)
			}

			var token string
			if tt.raw != nil {
				token = *tt.raw
			} else {
				base := tt.base
				if base == nil {
					base = validKubernetesToken
				}
				spec := base(f, time.Now())
				if tt.spoil != nil {
					tt.spoil(f, &spec)
				}
				token = MintToken(t, spec)
			}

			p, err := f.verifier.Verify(context.Background(), token)

			if tt.wantReason == "" {
				require.NoError(t, err)
				require.NotNil(t, p)
				if tt.check != nil {
					tt.check(t, p)
				}
				return
			}

			require.Error(t, err)
			assert.Nil(t, p)
			reason, ok := auth.ReasonOf(err)
			require.True(t, ok, "every failure must carry a reason: %v", err)
			assert.Equal(t, tt.wantReason, reason)

			issuerName, hasIssuer := auth.IssuerOf(err)
			assert.Equal(t, tt.wantIssuer != "", hasIssuer,
				"a failure carries an issuer name exactly when an entry was matched: %v", err)
			assert.Equal(t, tt.wantIssuer, issuerName)

			if tt.wantErrIs != nil {
				assert.ErrorIs(t, err, tt.wantErrIs)
			}
		})
	}
}

// What the parser makes of the claim set once the signature is out of the way:
// the audience, the two required claims, and the "sub".
func TestVerifyClaims(t *testing.T) {
	t.Parallel()

	runVerifyCases(t, []verifyCase{
		{
			name: "a real-shaped Kubernetes token verifies",
			check: func(t *testing.T, p *auth.Principal) {
				t.Helper()
				assert.Equal(t, k8sName, p.IssuerName)
				assert.Equal(t, k8sIssuer, p.Issuer)
				assert.Equal(t, admittedSubject, p.Subject)
				assert.Equal(t, []any{deploymentAudience}, p.Claims["aud"])
				assert.Contains(t, p.Claims, "kubernetes.io")
			},
		},
		{
			name: "a real-shaped SPIRE JWT-SVID verifies, with no nbf and its own audience",
			base: validSPIREToken,
			check: func(t *testing.T, p *auth.Principal) {
				t.Helper()
				assert.Equal(t, spireName, p.IssuerName)
				assert.Equal(t, spireSubject, p.Subject)
			},
		},
		{
			name:       "the wrong audience is rejected",
			spoil:      spoilAudience,
			wantReason: auth.ReasonAudienceMismatch,
			wantIssuer: k8sName,
			wantErrIs:  jwt.ErrTokenInvalidAudience,
		},
		// "aud" values that are present and resolve to nothing. golang-jwt raises
		// ErrTokenRequiredClaimMissing for each, so a reason picked by testing for
		// the key would report bad_signature. An absent claim, a number and an
		// object all reach the same clause as the empty array.
		{
			name:       "an empty aud array is audience_mismatch, not bad_signature",
			spoil:      func(_ fixture, spec *TokenSpec) { spec.Audience = []any{} },
			wantReason: auth.ReasonAudienceMismatch,
			wantIssuer: k8sName,
			wantErrIs:  jwt.ErrTokenRequiredClaimMissing,
		},
		{
			name:       "an empty aud string is audience_mismatch, not bad_signature",
			spoil:      func(_ fixture, spec *TokenSpec) { spec.Audience = "" },
			wantReason: auth.ReasonAudienceMismatch,
			wantIssuer: k8sName,
			wantErrIs:  jwt.ErrTokenRequiredClaimMissing,
		},

		// GetSubject reports an absent "sub" as ("", nil) and the parser does not
		// require the claim, so these would authenticate with an empty Subject. An
		// explicit "" reaches the same check.
		{
			name:       "a token with no sub is rejected",
			spoil:      func(_ fixture, spec *TokenSpec) { spec.Subject = "" },
			wantReason: auth.ReasonBadSignature,
			wantIssuer: k8sName,
		},
		{
			name:       "a token whose sub is not a string is rejected",
			spoil:      func(_ fixture, spec *TokenSpec) { spec.Extra["sub"] = 1234 },
			wantReason: auth.ReasonBadSignature,
			wantIssuer: k8sName,
		},
		{
			name:       "a token with no exp is rejected: golang-jwt would accept it by default",
			spoil:      func(_ fixture, spec *TokenSpec) { spec.ExpiresAt = nil },
			wantReason: auth.ReasonExpired,
			wantIssuer: k8sName,
			wantErrIs:  jwt.ErrTokenRequiredClaimMissing,
		},
		{
			name:       "an expired token is rejected as expired",
			spoil:      spoilExpiry,
			wantReason: auth.ReasonExpired,
			wantIssuer: k8sName,
			wantErrIs:  jwt.ErrTokenExpired,
		},
		{
			name: "nbf one second in the future is accepted: the controller pod is behind the apiserver",
			spoil: func(_ fixture, spec *TokenSpec) {
				now := time.Now().Add(time.Second)
				spec.IssuedAt, spec.NotBefore = At(now), At(now)
			},
		},
		{
			name: "nbf beyond the leeway is rejected as not_yet_valid, not as expired",
			spoil: func(_ fixture, spec *TokenSpec) {
				now := time.Now()
				spec.IssuedAt, spec.NotBefore = At(now.Add(time.Hour)), At(now.Add(time.Hour))
				spec.ExpiresAt = At(now.Add(2 * time.Hour))
			},
			wantReason: auth.ReasonNotYetValid,
			wantIssuer: k8sName,
			wantErrIs:  jwt.ErrTokenNotValidYet,
		},
	})
}

// Which entry a token selects, and which name the failure is attributed to.
func TestVerifyIssuerLookup(t *testing.T) {
	t.Parallel()

	runVerifyCases(t, []verifyCase{
		{
			name:       "an unknown issuer is rejected",
			spoil:      spoilIssuer,
			wantReason: auth.ReasonUnknownIssuer,
		},
		{
			// Lookup is an exact map hit, so an issuer extending a configured one
			// misses exactly as an absent or unrelated one does.
			name:       "an issuer that is a prefix of a configured one is rejected: lookup is exact",
			spoil:      func(_ fixture, spec *TokenSpec) { spec.Issuer = "https://kubernetes.default.svc" },
			wantReason: auth.ReasonUnknownIssuer,
		},
		{
			// The same failure against the other issuer, so the name is the matched
			// entry's and not a constant.
			name:       "a failure is attributed to the issuer that was actually matched",
			base:       validSPIREToken,
			spoil:      func(_ fixture, spec *TokenSpec) { spec.Audience = []any{deploymentAudience} },
			wantReason: auth.ReasonAudienceMismatch,
			wantIssuer: spireName,
			wantErrIs:  jwt.ErrTokenInvalidAudience,
		},
	})
}

// Everything between the credential and a verified claim set: the algorithm
// allow-list, key selection, the signature, and non-JWT credentials.
func TestVerifySignatureAndAlgorithm(t *testing.T) {
	t.Parallel()

	runVerifyCases(t, []verifyCase{
		{
			name:  "an ES256 token verifies for an issuer that allows it",
			base:  validSPIREToken,
			spoil: func(f fixture, spec *TokenSpec) { spec.Key = f.elliptic },
		},
		{
			name:    "a token with no kid verifies when the source serves one anyway",
			spoil:   func(_ fixture, spec *TokenSpec) { spec.OmitKeyID = true },
			prepare: func(f fixture) { f.k8sKeys.Keys[""] = f.signing.Public },
		},
		{
			name:       "an algorithm outside the issuer's list is rejected as an algorithm problem",
			spoil:      func(f fixture, spec *TokenSpec) { spec.Key = f.elliptic },
			wantReason: auth.ReasonAlgorithmNotAllowed,
			wantIssuer: k8sName,
		},
		{
			name: "alg none is rejected",
			spoil: func(_ fixture, spec *TokenSpec) {
				spec.Method = jwt.SigningMethodNone
				spec.Key.Private = jwt.UnsafeAllowNoneSignatureType
			},
			wantReason: auth.ReasonAlgorithmNotAllowed,
			wantIssuer: k8sName,
		},
		{
			name:       "a signature from the wrong key is rejected",
			spoil:      spoilSignature,
			wantReason: auth.ReasonBadSignature,
			wantIssuer: k8sName,
			wantErrIs:  jwt.ErrTokenSignatureInvalid,
		},
		{
			name:       "a kid the source does not serve is rejected",
			spoil:      func(_ fixture, spec *TokenSpec) { spec.KeyID = "no-such-kid" },
			wantReason: auth.ReasonBadSignature,
			wantIssuer: k8sName,
		},
		{
			name:       "a string that is not a JWT is rejected",
			raw:        rawCredential("not-a-jwt"),
			wantReason: auth.ReasonBadSignature,
			wantErrIs:  jwt.ErrTokenMalformed,
		},
		{
			name:       "an empty token is no_token, not bad_signature",
			raw:        rawCredential(""),
			wantReason: auth.ReasonNoToken,
		},
		{
			name:       "a stale key set is jwks_stale, not bad_signature",
			prepare:    func(f fixture) { f.k8sKeys.Err = auth.ErrKeysStale },
			wantReason: auth.ReasonJWKSStale,
			wantIssuer: k8sName,
			wantErrIs:  auth.ErrKeysStale,
		},
	})
}

// The middleware logs the underlying error on every rejection, and "iss" and
// "kid" are both read before anything is verified.
func TestErrorMessagesAreBoundedForUnauthenticatedCallers(t *testing.T) {
	t.Parallel()

	// Generous next to every message this package builds and tiny next to the
	// input, so it cannot pass by accident in either direction.
	const maxErrLength = 1024

	tests := []struct {
		name string
		// spoil injects the oversized value.
		spoil      func(f fixture, spec *TokenSpec)
		wantReason auth.FailureReason
	}{
		{
			name:       "a 100 KB iss, which is read before any issuer is matched",
			spoil:      func(_ fixture, spec *TokenSpec) { spec.Issuer = strings.Repeat("A", hugeBytes) },
			wantReason: auth.ReasonUnknownIssuer,
		},
		{
			// keyFunc runs before the signature is verified.
			name:       "a 100 KB kid, which keyFunc reads before the signature is checked",
			spoil:      func(_ fixture, spec *TokenSpec) { spec.KeyID = strings.Repeat("A", hugeBytes) },
			wantReason: auth.ReasonBadSignature,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)
			huge := strings.Repeat("A", hugeBytes)
			spec := validKubernetesToken(f, time.Now())
			tt.spoil(f, &spec)

			_, err := f.verifier.Authenticate(context.Background(), MintToken(t, spec))

			require.Error(t, err)
			reason, ok := auth.ReasonOf(err)
			require.True(t, ok)
			assert.Equal(t, tt.wantReason, reason, "the fix must not change which reason is reported")

			message := err.Error()
			assert.Less(t, len(message), maxErrLength,
				"one rejection wrote %d bytes; unbounded, a loop fills the node's disk", len(message))
			assert.NotContains(t, message, huge)
			assert.Contains(t, message, "truncated from 102400 bytes",
				"a truncated value must say so, or it reads as the whole one")
		})
	}

	t.Run("a 100 KB subject in the not_admitted message", func(t *testing.T) {
		t.Parallel()

		// Authenticated rather than anonymous, but the same class.
		f := newFixture(t)
		spec := validSPIREToken(f, time.Now())
		spec.Subject = "spiffe://example.org/tenant-workload/" + strings.Repeat("a", hugeBytes)

		_, err := f.verifier.Authenticate(context.Background(), MintToken(t, spec))

		require.Error(t, err)
		reason, ok := auth.ReasonOf(err)
		require.True(t, ok)
		assert.Equal(t, auth.ReasonNotAdmitted, reason)
		assert.Less(t, len(err.Error()), maxErrLength)
	})
}

// An override replaces the deployment-wide list, it does not add to it.
func TestVerifyAudienceOverrideMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		issuer     string
		subject    string
		audience   string
		wantReason auth.FailureReason
	}{
		{
			// The one direction no other test pins: an overriding issuer's audience
			// must not leak into the effective list of one that does not override.
			// The accepting directions are the happy paths of TestVerifyClaims.
			name:       "another issuer's audience is rejected for an issuer that does not override",
			issuer:     k8sIssuer,
			subject:    admittedSubject,
			audience:   spireAudience,
			wantReason: auth.ReasonAudienceMismatch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)
			now := time.Now()
			token := MintToken(t, TokenSpec{
				Key:       f.signing,
				Issuer:    tt.issuer,
				Subject:   tt.subject,
				Audience:  []any{tt.audience},
				IssuedAt:  At(now),
				ExpiresAt: At(now.Add(time.Hour)),
			})

			_, err := f.verifier.Verify(context.Background(), token)

			if tt.wantReason == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			reason, ok := auth.ReasonOf(err)
			require.True(t, ok)
			assert.Equal(t, tt.wantReason, reason)
		})
	}
}

// The 401/403 split: every verification failure is 401, only admission is 403.
func TestAuthenticate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		namespace  string
		wantReason auth.FailureReason
	}{
		{name: "an admitted service account authenticates", namespace: admittedNamespace},
		{
			name:       "a verified token from an unadmitted namespace is not_admitted",
			namespace:  "default",
			wantReason: auth.ReasonNotAdmitted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)
			spec := validKubernetesToken(f, time.Now())
			spec.Subject = "system:serviceaccount:" + tt.namespace + ":runner"
			spec.Extra = ServiceAccountClaims(tt.namespace, "runner")

			p, err := f.verifier.Authenticate(context.Background(), MintToken(t, spec))

			if tt.wantReason == "" {
				require.NoError(t, err)
				assert.Equal(t, k8sName, p.IssuerName)
				return
			}
			require.Error(t, err)
			assert.Nil(t, p)
			reason, ok := auth.ReasonOf(err)
			require.True(t, ok)
			assert.Equal(t, tt.wantReason, reason)

			// The token verified, so the controller knows exactly who signed it.
			issuerName, hasIssuer := auth.IssuerOf(err)
			require.True(t, hasIssuer, "a verified token's admission failure knows its issuer")
			assert.Equal(t, k8sName, issuerName)
		})
	}
}

// The issuer label is an operator-configured Issuer.Name or the constant
// "unknown". Nothing a caller puts in "iss" may reach it, or anyone with network
// reach to the API could mint unbounded Prometheus series.
func TestFailureIssuerNameComesFromTheRegistry(t *testing.T) {
	t.Parallel()

	// Every unregistered "iss" misses the same map lookup, so one value that looks
	// like the configured issuer stands for the class. The unbounded-length case
	// is TestErrorMessagesAreBoundedForUnauthenticatedCallers'.
	hostile := []string{hostileIssuer}

	f := newFixture(t)
	now := time.Now()
	for _, iss := range hostile {
		t.Run(iss[:min(len(iss), 40)], func(t *testing.T) {
			t.Parallel()

			spec := validKubernetesToken(f, now)
			spec.Issuer = iss

			_, err := f.verifier.Authenticate(context.Background(), MintToken(t, spec))

			require.Error(t, err)
			reason, ok := auth.ReasonOf(err)
			require.True(t, ok)
			assert.Equal(t, auth.ReasonUnknownIssuer, reason)

			name, hasIssuer := auth.IssuerOf(err)
			assert.False(t, hasIssuer, "an unregistered issuer is not an attributable issuer")
			assert.Empty(t, name, "the \"iss\" claim never becomes a label value")
		})
	}
}

func TestNewVerifier(t *testing.T) {
	t.Parallel()

	signing := RSAKey(t, "signing-1")
	source := NewStaticKeySource(signing)

	tests := []struct {
		name    string
		cfg     func() *auth.Config
		sources map[string]auth.KeySource
		wantErr string
	}{
		{
			name:    "both issuers wired",
			cfg:     testConfig,
			sources: map[string]auth.KeySource{k8sName: source, spireName: source},
		},
		{
			name:    "a configured issuer with no key source is a startup failure",
			cfg:     testConfig,
			sources: map[string]auth.KeySource{k8sName: source},
			wantErr: `issuer "spire"`,
		},
		{
			name:    "a nil key source is a startup failure",
			cfg:     testConfig,
			sources: map[string]auth.KeySource{k8sName: source, spireName: nil},
			wantErr: `issuer "spire"`,
		},
		{
			name:    "a key source for no configured issuer is a startup failure",
			cfg:     testConfig,
			sources: map[string]auth.KeySource{k8sName: source, spireName: source, "teleport": source},
			wantErr: `key source "teleport"`,
		},
		{
			name: "an invalid config is rejected here too, not only by Load",
			cfg: func() *auth.Config {
				cfg := testConfig()
				cfg.Audiences = []string{k8sIssuer}
				return cfg
			},
			sources: map[string]auth.KeySource{k8sName: source, spireName: source},
			wantErr: "invalid authentication config",
		},
		{
			name:    "a nil config is rejected",
			cfg:     func() *auth.Config { return nil },
			sources: map[string]auth.KeySource{},
			wantErr: "nil",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v, err := auth.NewVerifier(tt.cfg(), tt.sources)
			if tt.wantErr == "" {
				require.NoError(t, err)
				assert.NotNil(t, v)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Nil(t, v)
		})
	}
}

// TestErrorSurface pins the two things the middleware relies on: a reason it can
// label a metric with, and a cause it can log.
func TestErrorSurface(t *testing.T) {
	t.Parallel()

	t.Run("ReasonOf finds the reason through wrapping", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t)
		_, err := f.verifier.Verify(context.Background(), "")
		require.Error(t, err)

		reason, ok := auth.ReasonOf(err)
		require.True(t, ok)
		assert.Equal(t, auth.ReasonNoToken, reason)
		assert.Contains(t, err.Error(), string(auth.ReasonNoToken))
	})

	// A nil error takes the same errors.As miss.
	t.Run("ReasonOf reports absence for an unrelated error", func(t *testing.T) {
		t.Parallel()

		reason, ok := auth.ReasonOf(context.Canceled)
		assert.False(t, ok)
		assert.Empty(t, reason)
	})
}

// TestConfiguredAudiencesAreAnyOf: an operator who configures several audiences
// accepts a token carrying any one of them, and still rejects one carrying none.
// Any-of, not all-of. jwt.WithAllAudiences would require every configured
// audience to be present, so adding one would reject every token that does not
// also carry it.
func TestConfiguredAudiencesAreAnyOf(t *testing.T) {
	t.Parallel()

	const (
		first  = "https://cwe.example.com"
		second = "https://cwe.example.net"
	)

	signing := RSAKey(t, "signing-1")
	verifier, err := auth.NewVerifier(&auth.Config{
		Audiences: []string{first, second},
		Issuers: []auth.Issuer{{
			Name:       k8sName,
			Issuer:     k8sIssuer,
			JWKS:       &auth.JWKSKeys{URI: "https://kubernetes.default.svc/openid/v1/jwks"},
			Algorithms: []string{"RS256"},
			Admit: &auth.Admit{
				ServiceAccounts: []auth.ServiceAccountRule{{Namespace: admittedNamespace}},
			},
		}},
	}, map[string]auth.KeySource{k8sName: NewStaticKeySource(signing)})
	require.NoError(t, err)

	now := time.Now()
	mint := func(aud any) string {
		return MintToken(t, TokenSpec{
			Key:       signing,
			Issuer:    k8sIssuer,
			Subject:   admittedSubject,
			Audience:  aud,
			IssuedAt:  At(now),
			NotBefore: At(now),
			ExpiresAt: At(now.Add(time.Hour)),
			Extra:     ServiceAccountClaims(admittedNamespace, admittedName),
		})
	}

	tests := []struct {
		name string
		aud  any
		want bool
	}{
		// The discriminating row: a matched audience alongside an unmatched one is
		// what any-of accepts and all-of would reject. The negatives are
		// TestVerifyClaims'.
		{name: "one configured audience among others", aud: []any{"vault", second}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p, err := verifier.Verify(context.Background(), mint(tt.aud))
			if tt.want {
				require.NoError(t, err)
				assert.Equal(t, admittedSubject, p.Subject)
				return
			}
			require.Error(t, err)
			reason, ok := auth.ReasonOf(err)
			require.True(t, ok)
			assert.Equal(t, auth.ReasonAudienceMismatch, reason)
		})
	}
}
