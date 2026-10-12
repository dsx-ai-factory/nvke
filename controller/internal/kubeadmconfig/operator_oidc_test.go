// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationcel "k8s.io/apiserver/pkg/authentication/cel"
	"k8s.io/apiserver/pkg/cel/lazy"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
)

func TestBuildOperatorJWTAuthenticator(t *testing.T) {
	provider := controllerconfig.OIDCProvider{
		Name:           "workforce",
		IssuerURL:      "https://issuer.example.com",
		Audiences:      []string{"nke", "kubectl"},
		UsernameClaim:  "email",
		UsernamePrefix: "workforce:",
		GroupsClaim:    "groups",
		GroupsPrefix:   "team:",
		RequiredClaims: map[string]string{
			"tenant": "production",
			"hd":     "example.com",
		},
		CABundle: "-----BEGIN CERTIFICATE-----\nexample\n-----END CERTIFICATE-----\n",
	}

	got := mustBuildOperatorJWTAuthenticator(t, provider, OperatorJWTPolicy{})

	require.NotNil(t, got.ClaimMappings.Username.Prefix)
	require.NotNil(t, got.ClaimMappings.Groups.Prefix)
	assert.Equal(t, provider.IssuerURL, got.Issuer.URL)
	assert.Equal(t, provider.Audiences, got.Issuer.Audiences)
	assert.Equal(t, "MatchAny", string(got.Issuer.AudienceMatchPolicy))
	assert.Equal(t, provider.CABundle, got.Issuer.CertificateAuthority)
	assert.Equal(t, provider.UsernameClaim, got.ClaimMappings.Username.Claim)
	assert.Equal(t, provider.UsernamePrefix, *got.ClaimMappings.Username.Prefix)
	assert.Equal(t, provider.GroupsClaim, got.ClaimMappings.Groups.Claim)
	assert.Equal(t, provider.GroupsPrefix, *got.ClaimMappings.Groups.Prefix)
	assert.Equal(t, []OIDCClaimValidationRule{
		{Claim: "hd", RequiredValue: "example.com"},
		{Claim: "tenant", RequiredValue: "production"},
	}, got.ClaimValidationRules)
	require.NotEmpty(t, got.UserValidationRules)

	provider.Audiences[0] = "mutated"
	provider.RequiredClaims["hd"] = "mutated"
	assert.Equal(t, []string{"nke", "kubectl"}, got.Issuer.Audiences)
	assert.Equal(t, "example.com", got.ClaimValidationRules[0].RequiredValue)
}

func TestBuildOperatorJWTAuthenticatorOptionalGroupsAndEmptyPrefixes(t *testing.T) {
	tests := []struct {
		name           string
		groupsClaim    string
		groupsPrefix   string
		wantGroups     bool
		wantGroupsPref *string
	}{
		{
			name:         "groups omitted",
			groupsClaim:  "",
			groupsPrefix: "ignored:",
			wantGroups:   false,
		},
		{
			name:           "explicit empty group prefix",
			groupsClaim:    "roles",
			wantGroups:     true,
			wantGroupsPref: stringPtr(""),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mustBuildOperatorJWTAuthenticator(t, controllerconfig.OIDCProvider{
				IssuerURL:     "https://issuer.example.com",
				Audiences:     []string{"nke"},
				UsernameClaim: "sub",
				GroupsClaim:   tt.groupsClaim,
				GroupsPrefix:  tt.groupsPrefix,
			}, OperatorJWTPolicy{})

			require.NotNil(t, got.ClaimMappings.Username.Prefix)
			assert.Empty(t, *got.ClaimMappings.Username.Prefix)
			if !tt.wantGroups {
				assert.Empty(t, got.ClaimMappings.Groups)
				return
			}

			assert.Equal(t, tt.groupsClaim, got.ClaimMappings.Groups.Claim)
			require.NotNil(t, got.ClaimMappings.Groups.Prefix)
			assert.Equal(t, *tt.wantGroupsPref, *got.ClaimMappings.Groups.Prefix)
		})
	}
}

func TestOperatorJWTAuthenticatorReservedIdentityRules(t *testing.T) {
	prefixes := controllerconfig.ReservedIdentityPrefixes()
	usernameCases := []struct {
		name          string
		mappingPrefix func(string) string
		claim         func(string) string
	}{
		{
			name:          "claim contains prefix",
			mappingPrefix: func(string) string { return "" },
			claim:         func(prefix string) string { return prefix + "masters" },
		},
		{
			name:          "mapping contains prefix",
			mappingPrefix: func(prefix string) string { return prefix },
			claim:         func(string) string { return "masters" },
		},
		{
			name:          "mapping and claim split prefix",
			mappingPrefix: func(prefix string) string { return prefix[:len(prefix)/2] },
			claim:         func(prefix string) string { return prefix[len(prefix)/2:] + "masters" },
		},
	}

	for _, protectedPrefix := range prefixes {
		for _, usernameCase := range usernameCases {
			t.Run("username "+protectedPrefix+" "+usernameCase.name, func(t *testing.T) {
				provider := controllerconfig.OIDCProvider{
					IssuerURL:      "https://issuer.example.com",
					Audiences:      []string{"nke"},
					UsernameClaim:  "sub",
					UsernamePrefix: usernameCase.mappingPrefix(protectedPrefix),
				}
				authenticator := mustBuildOperatorJWTAuthenticator(t, provider, OperatorJWTPolicy{})

				assertUserValidation(t, authenticator.UserValidationRules, mapOperatorIdentity(authenticator, usernameCase.claim(protectedPrefix), nil), false)
				provider.UsernamePrefix = "people:"
				controlAuthenticator := mustBuildOperatorJWTAuthenticator(t, provider, OperatorJWTPolicy{})
				assertUserValidation(t, authenticator.UserValidationRules, mapOperatorIdentity(controlAuthenticator, "controller", nil), true)
			})
		}

		for position := range 3 {
			t.Run("group "+protectedPrefix+" position "+string(rune('1'+position)), func(t *testing.T) {
				claims := []string{"developers", "operators", "auditors"}
				claims[position] = protectedPrefix + "masters"
				provider := controllerconfig.OIDCProvider{
					IssuerURL:     "https://issuer.example.com",
					Audiences:     []string{"nke"},
					UsernameClaim: "sub",
					GroupsClaim:   "groups",
				}
				authenticator := mustBuildOperatorJWTAuthenticator(t, provider, OperatorJWTPolicy{})

				assertUserValidation(t, authenticator.UserValidationRules, mapOperatorIdentity(authenticator, "alice", claims), false)
				claims[position] = "security"
				assertUserValidation(t, authenticator.UserValidationRules, mapOperatorIdentity(authenticator, "alice", claims), true)
			})
		}
	}
}

func TestOperatorJWTAuthenticatorReservedIdentityRulesUseFinalMappedValues(t *testing.T) {
	tests := []struct {
		name           string
		usernamePrefix string
		usernameClaim  string
		groupsPrefix   string
		groupsClaims   []string
	}{
		{
			name:           "username prefix and claim combine into reserved name",
			usernamePrefix: "sys",
			usernameClaim:  "tem:masters",
		},
		{
			name:          "group prefix and claim combine into reserved group",
			usernameClaim: "alice",
			groupsPrefix:  "sys",
			groupsClaims:  []string{"tem:masters"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := controllerconfig.OIDCProvider{
				IssuerURL:      "https://issuer.example.com",
				Audiences:      []string{"nke"},
				UsernameClaim:  "sub",
				UsernamePrefix: tt.usernamePrefix,
				GroupsClaim:    "groups",
				GroupsPrefix:   tt.groupsPrefix,
			}
			authenticator := mustBuildOperatorJWTAuthenticator(t, provider, OperatorJWTPolicy{})

			assertUserValidation(t, authenticator.UserValidationRules, mapOperatorIdentity(authenticator, tt.usernameClaim, tt.groupsClaims), false)
			assertUserValidation(t, authenticator.UserValidationRules, mapOperatorIdentity(authenticator, "carol", []string{"developers"}), true)
		})
	}
}

func TestOperatorJWTAuthenticatorDoesNotReserveUnconfiguredSPIFFEIdentity(t *testing.T) {
	provider := controllerconfig.OIDCProvider{
		IssuerURL:     "https://issuer.example.com",
		Audiences:     []string{"nke"},
		UsernameClaim: "sub",
		GroupsClaim:   "groups",
	}
	authenticator := mustBuildOperatorJWTAuthenticator(t, provider, OperatorJWTPolicy{})

	assertUserValidation(t, authenticator.UserValidationRules, mapOperatorIdentity(
		authenticator,
		"spiffe://example.org/nke/controller",
		[]string{"spiffe://example.org/nke/operators"},
	), true)
}

func TestOperatorJWTAuthenticatorSPIFFEPolicy(t *testing.T) {
	provider := controllerconfig.OIDCProvider{
		IssuerURL:     "https://issuer.example.com",
		Audiences:     []string{"nke"},
		UsernameClaim: "sub",
		GroupsClaim:   "groups",
	}

	t.Run("configured trust domain", func(t *testing.T) {
		authenticator := mustBuildOperatorJWTAuthenticator(t, provider, OperatorJWTPolicy{
			ManagementJWTTrustDomain: "example.org",
		})
		identities := []struct {
			name     string
			username string
			groups   []string
			allowed  bool
		}{
			{name: "SPIFFE username", username: "spiffe://example.org/nke/controller", allowed: false},
			{name: "SPIFFE group first", username: "alice", groups: []string{"spiffe://example.org/nke/admins", "operators", "auditors"}, allowed: false},
			{name: "SPIFFE group middle", username: "alice", groups: []string{"developers", "spiffe://example.org/nke/admins", "auditors"}, allowed: false},
			{name: "SPIFFE group last", username: "alice", groups: []string{"developers", "operators", "spiffe://example.org/nke/admins"}, allowed: false},
			{name: "other trust domain", username: "spiffe://other.example.org/nke/controller", groups: []string{"developers"}, allowed: true},
			{name: "lookalike trust domain", username: "spiffe://example.org.evil/nke/controller", groups: []string{"developers"}, allowed: true},
			{name: "other trust domain group", username: "alice", groups: []string{"spiffe://other.example.org/nke/operators"}, allowed: true},
			{name: "lookalike trust domain group", username: "alice", groups: []string{"spiffe://example.org.evil/nke/operators"}, allowed: true},
		}

		for _, tt := range identities {
			t.Run(tt.name, func(t *testing.T) {
				assertUserValidation(t, authenticator.UserValidationRules, mappedOperatorIdentity{username: tt.username, groups: tt.groups}, tt.allowed)
				if !tt.allowed {
					assertUserValidation(t, authenticator.UserValidationRules, mappedOperatorIdentity{
						username: "alice",
						groups:   []string{"developers", "operators", "auditors"},
					}, true)
				}
			})
		}

		for _, prefix := range controllerconfig.ReservedIdentityPrefixes() {
			t.Run("fixed prefix "+prefix, func(t *testing.T) {
				assertUserValidation(t, authenticator.UserValidationRules, mappedOperatorIdentity{username: prefix + "controller"}, false)
				assertUserValidation(t, authenticator.UserValidationRules, mappedOperatorIdentity{
					username: "alice",
					groups:   []string{"developers", prefix + "admins"},
				}, false)
				assertUserValidation(t, authenticator.UserValidationRules, mappedOperatorIdentity{
					username: "alice",
					groups:   []string{"developers", "operators"},
				}, true)
			})
		}
	})

	t.Run("no trust domain", func(t *testing.T) {
		authenticator := mustBuildOperatorJWTAuthenticator(t, provider, OperatorJWTPolicy{})

		assertUserValidation(t, authenticator.UserValidationRules, mappedOperatorIdentity{
			username: "spiffe://example.org/nke/controller",
			groups:   []string{"spiffe://example.org/nke/operators"},
		}, true)
		assertUserValidation(t, authenticator.UserValidationRules, mappedOperatorIdentity{username: "system:masters"}, false)
	})
}

func TestOperatorJWTAuthenticatorSPIFFERulesUseFinalMappedValues(t *testing.T) {
	tests := []struct {
		name           string
		usernamePrefix string
		usernameClaim  string
		groupsPrefix   string
		groupsClaims   []string
	}{
		{
			name:           "username prefix and claim combine into reserved SPIFFE ID",
			usernamePrefix: "spiffe://exam",
			usernameClaim:  "ple.org/nke/controller",
		},
		{
			name:          "group prefix and claim combine into reserved SPIFFE ID",
			usernameClaim: "alice",
			groupsPrefix:  "spiffe://exam",
			groupsClaims:  []string{"ple.org/nke/operators"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := controllerconfig.OIDCProvider{
				IssuerURL:      "https://issuer.example.com",
				Audiences:      []string{"nke"},
				UsernameClaim:  "sub",
				UsernamePrefix: tt.usernamePrefix,
				GroupsClaim:    "groups",
				GroupsPrefix:   tt.groupsPrefix,
			}
			authenticator := mustBuildOperatorJWTAuthenticator(t, provider, OperatorJWTPolicy{
				ManagementJWTTrustDomain: "example.org",
			})

			assertUserValidation(t, authenticator.UserValidationRules, mapOperatorIdentity(authenticator, tt.usernameClaim, tt.groupsClaims), false)
			assertUserValidation(t, authenticator.UserValidationRules, mapOperatorIdentity(authenticator, "alice", []string{"developers"}), true)
		})
	}
}

func TestBuildOperatorJWTAuthenticatorTrustDomainValidation(t *testing.T) {
	provider := controllerconfig.OIDCProvider{
		IssuerURL:     "https://issuer.example.com",
		Audiences:     []string{"nke"},
		UsernameClaim: "sub",
	}
	invalid := []string{
		"spiffe://example.org",
		"spiffe://example.org/nke/controller",
		"https://example.org",
		"example.org/path",
		" example.org",
		"example.org ",
		"EXAMPLE.org",
		"example org",
		"example.org:443",
		`example.org') || true || ('`,
	}

	for _, trustDomain := range invalid {
		t.Run(trustDomain, func(t *testing.T) {
			got, err := BuildOperatorJWTAuthenticator(provider, OperatorJWTPolicy{
				ManagementJWTTrustDomain: trustDomain,
			})
			require.Error(t, err)
			assert.Empty(t, got)
		})
	}

	t.Run("valid", func(t *testing.T) {
		_, err := BuildOperatorJWTAuthenticator(provider, OperatorJWTPolicy{
			ManagementJWTTrustDomain: "example.org",
		})
		require.NoError(t, err)
	})

	t.Run("empty skips trust domain rule", func(t *testing.T) {
		_, err := BuildOperatorJWTAuthenticator(provider, OperatorJWTPolicy{})
		require.NoError(t, err)
	})
}

func TestOperatorJWTAuthenticatorReservedSubjects(t *testing.T) {
	provider := controllerconfig.OIDCProvider{
		IssuerURL:     "https://issuer.example.com",
		Audiences:     []string{"nke"},
		UsernameClaim: "sub",
		GroupsClaim:   "groups",
	}
	reservedSubjects := []string{
		"spiffe://exchange.example/nke-service/capi-manager",
		`spiffe://exchange.example/nke-service/cwe-\"quoted\"`,
	}
	authenticator := mustBuildOperatorJWTAuthenticator(t, provider, OperatorJWTPolicy{
		ReservedSubjects: reservedSubjects,
	})

	for _, subject := range reservedSubjects {
		t.Run(subject, func(t *testing.T) {
			assertUserValidation(t, authenticator.UserValidationRules, mappedOperatorIdentity{username: subject}, false)
			assertUserValidation(t, authenticator.UserValidationRules, mappedOperatorIdentity{
				username: "alice",
				groups:   []string{"developers", subject, "auditors"},
			}, false)
		})
	}

	assertUserValidation(t, authenticator.UserValidationRules, mappedOperatorIdentity{
		username: "spiffe://exchange.example/nke-service/other",
		groups:   []string{"spiffe://exchange.example/nke-service/operators"},
	}, true)
}

func TestBuildOperatorJWTAuthenticatorReservedSubjectValidation(t *testing.T) {
	provider := controllerconfig.OIDCProvider{
		IssuerURL:     "https://issuer.example.com",
		Audiences:     []string{"nke"},
		UsernameClaim: "sub",
	}
	tests := []struct {
		name     string
		subjects []string
		index    int
	}{
		{name: "empty", subjects: []string{""}},
		{name: "whitespace", subjects: []string{"spiffe://exchange.example/nke-service/capi-manager", " \t\n"}, index: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildOperatorJWTAuthenticator(provider, OperatorJWTPolicy{ReservedSubjects: tt.subjects})

			require.ErrorContains(t, err, fmt.Sprintf("reserved subject at index %d must not be empty", tt.index))
			assert.Empty(t, got)
		})
	}
}

func TestInjectReservedNamespaceValidationPreservesExistingRules(t *testing.T) {
	existing := OIDCUserValidationRule{
		Expression: "user.username != 'blocked'",
		Message:    "blocked user",
	}
	authenticator := OIDCProvider{UserValidationRules: []OIDCUserValidationRule{existing}}

	err := injectReservedNamespaceValidation(&authenticator, OperatorJWTPolicy{
		ManagementJWTTrustDomain: "example.org",
	})

	require.NoError(t, err)
	require.Greater(t, len(authenticator.UserValidationRules), 1)
	assert.Equal(t, existing, authenticator.UserValidationRules[0])
	assertUserValidation(t, authenticator.UserValidationRules, mappedOperatorIdentity{username: "blocked"}, false)
	assertUserValidation(t, authenticator.UserValidationRules, mappedOperatorIdentity{username: "alice"}, true)

	beforeInvalidPolicy := append([]OIDCUserValidationRule(nil), authenticator.UserValidationRules...)
	err = injectReservedNamespaceValidation(&authenticator, OperatorJWTPolicy{
		ManagementJWTTrustDomain: "spiffe://example.org/nke/controller",
	})
	require.Error(t, err)
	assert.Equal(t, beforeInvalidPolicy, authenticator.UserValidationRules)
}

func TestBuildOIDCAuthenticationConfigSharedProviderUnchanged(t *testing.T) {
	caBundle := []byte("-----BEGIN CERTIFICATE-----\nshared\n-----END CERTIFICATE-----\n")
	_ = mustBuildOperatorJWTAuthenticator(t, controllerconfig.OIDCProvider{
		IssuerURL:     "https://issuer.example.com",
		Audiences:     []string{"nke"},
		UsernameClaim: "sub",
	}, OperatorJWTPolicy{
		ManagementJWTTrustDomain: "example.org",
	})

	got, err := BuildOIDCAuthenticationConfig("tenant-42", config.NvkeOIDCAuth{
		IssuerURL:    "https://shared.example.com",
		DiscoveryURL: "https://discovery.example.com/.well-known/openid-configuration",
	}, caBundle)

	require.NoError(t, err)
	require.Len(t, got.JWT, 1)
	assert.Empty(t, got.JWT[0].UserValidationRules)
	assert.Equal(t, OIDCProvider{
		Issuer: OIDCIssuer{
			URL:                  "https://shared.example.com",
			DiscoveryURL:         stringPtr("https://discovery.example.com/.well-known/openid-configuration"),
			CertificateAuthority: string(caBundle),
			Audiences:            []string{"tenant-42"},
		},
		ClaimMappings: OIDCClaimMappings{
			Username: OIDCPrefixedClaimOrExpression{Claim: "sub", Prefix: stringPtr("")},
		},
	}, got.JWT[0])
}

type mappedOperatorIdentity struct {
	username string
	groups   []string
}

func mapOperatorIdentity(authenticator OIDCProvider, usernameClaim string, groupsClaims []string) mappedOperatorIdentity {
	identity := mappedOperatorIdentity{username: *authenticator.ClaimMappings.Username.Prefix + usernameClaim}
	for _, group := range groupsClaims {
		identity.groups = append(identity.groups, *authenticator.ClaimMappings.Groups.Prefix+group)
	}
	return identity
}

func mustBuildOperatorJWTAuthenticator(t *testing.T, provider controllerconfig.OIDCProvider, policy OperatorJWTPolicy) OIDCProvider {
	t.Helper()

	authenticator, err := BuildOperatorJWTAuthenticator(provider, policy)
	require.NoError(t, err)
	return authenticator
}

func assertUserValidation(t *testing.T, rules []OIDCUserValidationRule, identity mappedOperatorIdentity, wantAllowed bool) {
	t.Helper()
	require.NotEmpty(t, rules)

	compiler := authenticationcel.NewDefaultCompiler()
	compiled := make([]authenticationcel.CompilationResult, 0, len(rules))
	for _, rule := range rules {
		result, err := compiler.CompileUserExpression(&authenticationcel.UserValidationCondition{
			Expression: rule.Expression,
			Message:    rule.Message,
		})
		require.NoError(t, err)
		compiled = append(compiled, result)
	}

	userInfo := lazy.NewMapValue(types.NewObjectType("kubernetes.UserInfo"))
	userInfo.Append("username", func(*lazy.MapValue) ref.Val {
		return types.String(identity.username)
	})
	userInfo.Append("uid", func(*lazy.MapValue) ref.Val {
		return types.String("")
	})
	userInfo.Append("groups", func(*lazy.MapValue) ref.Val {
		return types.DefaultTypeAdapter.NativeToValue(identity.groups)
	})
	userInfo.Append("extra", func(*lazy.MapValue) ref.Val {
		return types.DefaultTypeAdapter.NativeToValue(map[string][]string{})
	})

	results, err := authenticationcel.NewUserMapper(compiled).EvalUser(context.Background(), userInfo)
	require.NoError(t, err)
	allowed := true
	for _, result := range results {
		require.Equal(t, types.BoolType, result.EvalResult.Type())
		allowed = allowed && result.EvalResult.Value().(bool)
	}
	assert.Equal(t, wantAllowed, allowed)
}
