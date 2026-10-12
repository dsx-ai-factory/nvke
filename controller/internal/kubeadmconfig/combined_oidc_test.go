// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiserverv1 "k8s.io/apiserver/pkg/apis/apiserver/v1"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
)

func TestBuildCombinedAuthenticationConfig(t *testing.T) {
	shared := controllerconfig.ManagementOIDCAuthConfig{
		Enabled:      true,
		IssuerURL:    "https://management.example.com",
		DiscoveryURL: "https://management.example.com/.well-known/openid-configuration",
	}
	operatorOne := combinedTestProvider("operator-one")
	operatorTwo := combinedTestProvider("operator-two")
	disabledShared := shared
	disabledShared.Enabled = false
	matchingOperator := combinedTestProvider("workforce")
	matchingOperator.IssuerURL = shared.IssuerURL

	tests := []struct {
		name        string
		management  controllerconfig.ManagementOIDCAuthConfig
		providers   []controllerconfig.OIDCProvider
		policy      OperatorJWTPolicy
		wantIssuers []string
		wantShared  bool
	}{
		{
			name:        "shared provider only",
			management:  shared,
			policy:      OperatorJWTPolicy{ManagementJWTTrustDomain: "jwt.example.org"},
			wantIssuers: []string{"https://management.example.com"},
			wantShared:  true,
		},
		{
			name:        "operator provider reserves trust domain without shared provider",
			providers:   []controllerconfig.OIDCProvider{operatorOne},
			policy:      OperatorJWTPolicy{ManagementJWTTrustDomain: "jwt.example.org"},
			wantIssuers: []string{"https://operator-one.example.com"},
		},
		{
			name:        "disabled shared provider allows matching operator issuer",
			management:  disabledShared,
			providers:   []controllerconfig.OIDCProvider{operatorOne, matchingOperator},
			wantIssuers: []string{"https://operator-one.example.com", shared.IssuerURL},
		},
		{
			name:        "several operator providers preserve input order",
			providers:   []controllerconfig.OIDCProvider{operatorTwo, operatorOne},
			wantIssuers: []string{"https://operator-two.example.com", "https://operator-one.example.com"},
		},
		{
			name:       "shared provider precedes operator providers",
			management: shared,
			providers:  []controllerconfig.OIDCProvider{operatorTwo, operatorOne},
			policy: OperatorJWTPolicy{
				ManagementJWTTrustDomain: "jwt.example.org",
				ReservedSubjects: []string{
					"spiffe://exchange.example/nke-service/capi-manager",
					"spiffe://exchange.example/nke-service/capd-manager",
				},
			},
			wantIssuers: []string{"https://management.example.com", "https://operator-two.example.com", "https://operator-one.example.com"},
			wantShared:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildCombinedAuthenticationConfig("tenant-cluster", tt.management, []byte("shared CA"), tt.providers, tt.policy)
			require.NoError(t, err)
			require.Len(t, got.JWT, len(tt.wantIssuers))

			for i, wantIssuer := range tt.wantIssuers {
				assert.Equal(t, wantIssuer, got.JWT[i].Issuer.URL)
			}

			operatorOffset := 0
			if tt.wantShared {
				operatorOffset = 1
				assert.Empty(t, got.JWT[0].UserValidationRules)
				assert.Equal(t, "shared CA", got.JWT[0].Issuer.CertificateAuthority)
				assert.Equal(t, []string{"tenant-cluster"}, got.JWT[0].Issuer.Audiences)
				require.NotNil(t, got.JWT[0].Issuer.DiscoveryURL)
				assert.Equal(t, shared.DiscoveryURL, *got.JWT[0].Issuer.DiscoveryURL)
				assert.Equal(t, "sub", got.JWT[0].ClaimMappings.Username.Claim)
				require.NotNil(t, got.JWT[0].ClaimMappings.Username.Prefix)
				assert.Empty(t, *got.JWT[0].ClaimMappings.Username.Prefix)
			}

			for i := operatorOffset; i < len(got.JWT); i++ {
				assertCombinedFixedPrefixRules(t, got.JWT[i])
				wantRuleCount := 2 + 2*len(tt.policy.ReservedSubjects)
				if tt.policy.ManagementJWTTrustDomain != "" {
					wantRuleCount += 2
				}
				assert.Len(t, got.JWT[i].UserValidationRules, wantRuleCount)
				identity := mappedOperatorIdentity{
					username: "spiffe://jwt.example.org/nke/controller",
					groups:   []string{"spiffe://jwt.example.org/nke/operators"},
				}
				assertUserValidation(t, got.JWT[i].UserValidationRules, identity, tt.policy.ManagementJWTTrustDomain == "")
				for _, subject := range tt.policy.ReservedSubjects {
					assertUserValidation(t, got.JWT[i].UserValidationRules, mappedOperatorIdentity{username: subject}, false)
					assertUserValidation(t, got.JWT[i].UserValidationRules, mappedOperatorIdentity{
						username: "alice",
						groups:   []string{"developers", subject},
					}, false)
				}
			}
		})
	}
}

func TestBuildCombinedAuthenticationConfigProviderLimit(t *testing.T) {
	tests := []struct {
		name          string
		management    controllerconfig.ManagementOIDCAuthConfig
		operatorCount int
		wantCount     int
		wantError     bool
	}{
		{name: "exactly 64 operators", operatorCount: 64, wantCount: 64},
		{name: "65 operators", operatorCount: 65, wantError: true},
		{
			name:          "shared provider plus 63 operators",
			management:    combinedTestManagementProvider(),
			operatorCount: 63,
			wantCount:     64,
		},
		{
			name:          "shared provider plus 64 operators",
			management:    combinedTestManagementProvider(),
			operatorCount: 64,
			wantError:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			providers := make([]controllerconfig.OIDCProvider, tt.operatorCount)
			for i := range providers {
				providers[i] = combinedTestProvider(fmt.Sprintf("operator-%d", i))
			}

			policy := OperatorJWTPolicy{}
			if tt.management.Enabled {
				policy.ManagementJWTTrustDomain = "jwt.example.org"
			}
			got, err := BuildCombinedAuthenticationConfig("tenant-cluster", tt.management, nil, providers, policy)
			if tt.wantError {
				require.Error(t, err)
				require.ErrorContains(t, err, "64")
				assert.Nil(t, got)
				return
			}

			require.NoError(t, err)
			assert.Len(t, got.JWT, tt.wantCount)
		})
	}
}

func TestBuildCombinedAuthenticationConfigErrors(t *testing.T) {
	matchingOperator := combinedTestProvider("workforce")
	matchingOperator.IssuerURL = combinedTestManagementProvider().IssuerURL
	matchingOperator.Audiences = []string{"operator-client"}

	tests := []struct {
		name        string
		clusterName string
		management  controllerconfig.ManagementOIDCAuthConfig
		providers   []controllerconfig.OIDCProvider
		policy      OperatorJWTPolicy
		want        []string
	}{
		{
			name: "no providers",
			want: []string{"at least one"},
		},
		{
			name:        "shared provider construction error",
			clusterName: "tenant-cluster",
			management: controllerconfig.ManagementOIDCAuthConfig{
				Enabled: true,
			},
			policy: OperatorJWTPolicy{ManagementJWTTrustDomain: "jwt.example.org"},
			want:   []string{"shared provider", "issuer"},
		},
		{
			name:        "shared provider requires management trust domain",
			clusterName: "tenant-cluster",
			management:  combinedTestManagementProvider(),
			want:        []string{"shared provider", "trust domain"},
		},
		{
			name:        "shared provider rejects invalid management trust domain",
			clusterName: "tenant-cluster",
			management:  combinedTestManagementProvider(),
			policy:      OperatorJWTPolicy{ManagementJWTTrustDomain: "spiffe://management.example.com/nke/controller"},
			want:        []string{"shared provider", "trust domain"},
		},
		{
			name:        "shared issuer collision with different audience",
			clusterName: "tenant-cluster",
			management:  combinedTestManagementProvider(),
			providers:   []controllerconfig.OIDCProvider{matchingOperator},
			policy:      OperatorJWTPolicy{ManagementJWTTrustDomain: "jwt.example.org"},
			want:        []string{`operator provider at index 0 ("workforce")`, "issuer URL", matchingOperator.IssuerURL, "shared provider"},
		},
		{
			name:        "shared issuer collision after a distinct operator",
			clusterName: "tenant-cluster",
			management:  combinedTestManagementProvider(),
			providers:   []controllerconfig.OIDCProvider{combinedTestProvider("operator-one"), matchingOperator},
			policy:      OperatorJWTPolicy{ManagementJWTTrustDomain: "jwt.example.org"},
			want:        []string{`operator provider at index 1 ("workforce")`, "issuer URL", matchingOperator.IssuerURL, "shared provider"},
		},
		{
			name:        "operator provider error identifies index and name",
			clusterName: "tenant-cluster",
			management:  combinedTestManagementProvider(),
			providers:   []controllerconfig.OIDCProvider{combinedTestProvider("workforce")},
			policy:      OperatorJWTPolicy{ManagementJWTTrustDomain: "invalid trust domain"},
			want:        []string{"operator provider", "0", "workforce", "trust domain"},
		},
		{
			name:        "operator provider rejects invalid trust domain without shared provider",
			clusterName: "tenant-cluster",
			providers:   []controllerconfig.OIDCProvider{combinedTestProvider("workforce")},
			policy:      OperatorJWTPolicy{ManagementJWTTrustDomain: "invalid trust domain"},
			want:        []string{"operator provider", "0", "workforce", "trust domain"},
		},
		{
			name:        "operator provider requires management trust domain",
			clusterName: "tenant-cluster",
			management:  combinedTestManagementProvider(),
			providers:   []controllerconfig.OIDCProvider{combinedTestProvider("workforce")},
			want:        []string{"operator provider", "0", "workforce", "trust domain"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildCombinedAuthenticationConfig(tt.clusterName, tt.management, nil, tt.providers, tt.policy)
			require.Error(t, err)
			assert.Nil(t, got)
			for _, want := range tt.want {
				require.ErrorContains(t, err, want)
			}
		})
	}
}

func TestBuildCombinedAuthenticationConfigDoesNotAliasCallerInputs(t *testing.T) {
	management := combinedTestManagementProvider()
	sharedCA := []byte("shared CA")
	providers := []controllerconfig.OIDCProvider{
		{
			Name:           "workforce",
			IssuerURL:      "https://workforce.example.com",
			Audiences:      []string{"tenant-cluster", "kubectl"},
			UsernameClaim:  "email",
			UsernamePrefix: "workforce:",
			RequiredClaims: map[string]string{"tenant": "production"},
			CABundle:       "operator CA",
		},
	}
	wantProviders := cloneCombinedTestProviders(providers)
	wantManagement := management

	got, err := BuildCombinedAuthenticationConfig("tenant-cluster", management, sharedCA, providers, OperatorJWTPolicy{
		ManagementJWTTrustDomain: "jwt.example.org",
	})
	require.NoError(t, err)
	assert.Equal(t, wantManagement, management)
	assert.Equal(t, wantProviders, providers)
	assert.Equal(t, []byte("shared CA"), sharedCA)

	sharedCA[0] = 'X'
	providers[0].Audiences[0] = "changed by caller"
	providers[0].RequiredClaims["tenant"] = "changed by caller"
	assert.Equal(t, "shared CA", got.JWT[0].Issuer.CertificateAuthority)
	assert.Equal(t, []string{"tenant-cluster", "kubectl"}, got.JWT[1].Issuer.Audiences)
	assert.Equal(t, "production", got.JWT[1].ClaimValidationRules[0].RequiredValue)

	callerAfterChanges := cloneCombinedTestProviders(providers)
	got.JWT[1].Issuer.Audiences[1] = "changed in result"
	got.JWT[1].ClaimValidationRules[0].RequiredValue = "changed in result"
	assert.Equal(t, callerAfterChanges, providers)
}

func TestBuildCombinedAuthenticationConfigRenders(t *testing.T) {
	config, err := BuildCombinedAuthenticationConfig(
		"tenant-cluster",
		combinedTestManagementProvider(),
		[]byte("shared CA"),
		[]controllerconfig.OIDCProvider{combinedTestProvider("workforce")},
		OperatorJWTPolicy{ManagementJWTTrustDomain: "jwt.example.org"},
	)
	require.NoError(t, err)

	file, err := GenerateAPIServerAuthenticationConfig(config, supportedK8sVersion)
	require.NoError(t, err)
	assert.Equal(t, defaultOIDCAuthConfigPath, file.Path)

	var parsed apiserverv1.AuthenticationConfiguration
	require.NoError(t, yaml.Unmarshal(file.Content, &parsed))
	assert.Equal(t, apiserverv1.SchemeGroupVersion.String(), parsed.APIVersion)
	assert.Equal(t, authenticationConfigurationKind, parsed.Kind)
	assert.Equal(t, config.JWT, parsed.JWT)
	require.NotNil(t, parsed.Anonymous)
	assert.True(t, parsed.Anonymous.Enabled)
	assert.Equal(t, []apiserverv1.AnonymousAuthCondition{
		{Path: "/livez"},
		{Path: "/readyz"},
		{Path: "/healthz"},
		{Path: "/version"},
		{Path: "/version/"},
		{Path: "/api/v1/namespaces/kube-public/configmaps/cluster-info"},
	}, parsed.Anonymous.Conditions)
	assert.Nil(t, config.Anonymous)
}

func combinedTestManagementProvider() controllerconfig.ManagementOIDCAuthConfig {
	return controllerconfig.ManagementOIDCAuthConfig{
		Enabled:      true,
		IssuerURL:    "https://management.example.com",
		DiscoveryURL: "https://management.example.com/.well-known/openid-configuration",
	}
}

func combinedTestProvider(name string) controllerconfig.OIDCProvider {
	return controllerconfig.OIDCProvider{
		Name:           name,
		IssuerURL:      "https://" + name + ".example.com",
		Audiences:      []string{"tenant-cluster"},
		UsernameClaim:  "sub",
		UsernamePrefix: name + ":",
		GroupsClaim:    "groups",
		GroupsPrefix:   name + ":",
		RequiredClaims: map[string]string{"tenant": "production"},
		CABundle:       name + " CA",
	}
}

func cloneCombinedTestProviders(providers []controllerconfig.OIDCProvider) []controllerconfig.OIDCProvider {
	cloned := make([]controllerconfig.OIDCProvider, len(providers))
	for i := range providers {
		cloned[i] = providers[i]
		cloned[i].Audiences = append([]string(nil), providers[i].Audiences...)
		if providers[i].RequiredClaims != nil {
			cloned[i].RequiredClaims = make(map[string]string, len(providers[i].RequiredClaims))
			for claim, value := range providers[i].RequiredClaims {
				cloned[i].RequiredClaims[claim] = value
			}
		}
	}
	return cloned
}

func assertCombinedFixedPrefixRules(t *testing.T, authenticator OIDCProvider) {
	t.Helper()
	for _, prefix := range controllerconfig.ReservedIdentityPrefixes() {
		assertUserValidation(t, authenticator.UserValidationRules, mappedOperatorIdentity{username: prefix + "controller"}, false)
		assertUserValidation(t, authenticator.UserValidationRules, mappedOperatorIdentity{
			username: "alice",
			groups:   []string{"developers", prefix + "admins"},
		}, false)
	}
}
