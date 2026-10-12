// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiserver "k8s.io/apiserver/pkg/apis/apiserver"
	apiserverv1 "k8s.io/apiserver/pkg/apis/apiserver/v1"
	kubeoidc "k8s.io/apiserver/plugin/pkg/authenticator/token/oidc"

	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
)

func TestOperatorJWTAuthenticatorAuthenticatesSignedTokens(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	require.NoError(t, err)
	keySet := operatorJWTStaticKeySet{publicKey: &key.PublicKey}
	policy := OperatorJWTPolicy{
		ManagementJWTTrustDomain: "example.org",
		ReservedSubjects:         []string{"spiffe://exchange.example/nke-service/capi-manager"},
	}

	type identityCase struct {
		name           string
		usernamePrefix string
		usernameClaim  string
		groupsPrefix   string
		groupsClaims   []string
		wantUsername   string
		wantGroups     []string
		wantError      string
	}
	tests := []identityCase{
		{
			name: "allowed unprefixed identity", usernameClaim: "alice", groupsClaims: []string{"developers"},
			wantUsername: "alice", wantGroups: []string{"developers"},
		},
		{
			name: "allowed prefixed identity", usernamePrefix: "people:", usernameClaim: "alice",
			groupsPrefix: "team:", groupsClaims: []string{"developers", "auditors"},
			wantUsername: "people:alice", wantGroups: []string{"team:developers", "team:auditors"},
		},
		{
			name: "safe prefixes on reserved claims", usernamePrefix: "people:", usernameClaim: "system:masters",
			groupsPrefix: "team:", groupsClaims: []string{"nvidia:admins"},
			wantUsername: "people:system:masters", wantGroups: []string{"team:nvidia:admins"},
		},
		{
			name: "other SPIFFE trust domain", usernameClaim: "spiffe://other.example.org/nke/controller",
			groupsClaims: []string{"spiffe://other.example.org/nke/operators"},
			wantUsername: "spiffe://other.example.org/nke/controller", wantGroups: []string{"spiffe://other.example.org/nke/operators"},
		},
		{
			name: "lookalike SPIFFE trust domain", usernameClaim: "spiffe://example.org.evil/nke/controller",
			groupsClaims: []string{"spiffe://example.org.evil/nke/operators"},
			wantUsername: "spiffe://example.org.evil/nke/controller", wantGroups: []string{"spiffe://example.org.evil/nke/operators"},
		},
		{
			name: "reserved subject suffix is allowed", usernameClaim: "spiffe://exchange.example/nke-service/capi-manager-other",
			groupsClaims: []string{"spiffe://exchange.example/nke-service/capi-manager-other"},
			wantUsername: "spiffe://exchange.example/nke-service/capi-manager-other",
			wantGroups:   []string{"spiffe://exchange.example/nke-service/capi-manager-other"},
		},
	}
	reservedIdentities := []struct {
		name      string
		value     string
		namespace string
		split     int
	}{
		{name: "system namespace", value: "system:masters", namespace: "system:", split: 3},
		{name: "kubeadm namespace", value: "kubeadm:masters", namespace: "kubeadm:", split: 3},
		{name: "nke namespace", value: "nke:masters", namespace: "nke:", split: 2},
		{name: "nvidia namespace", value: "nvidia:masters", namespace: "nvidia:", split: 3},
		{name: "SPIFFE trust domain", value: "spiffe://example.org/nke/controller", namespace: "spiffe://example.org/", split: 13},
		{name: "exact reserved subject", value: "spiffe://exchange.example/nke-service/capi-manager", namespace: "spiffe://exchange.example/nke-service/", split: 13},
	}
	for _, reserved := range reservedIdentities {
		mappings := []struct {
			name   string
			prefix string
			claim  string
		}{
			{name: "claim", claim: reserved.value},
			{name: "split prefix and claim", prefix: reserved.value[:reserved.split], claim: reserved.value[reserved.split:]},
			{name: "namespace in prefix", prefix: reserved.namespace, claim: reserved.value[len(reserved.namespace):]},
		}
		for _, mapping := range mappings {
			tests = append(tests,
				identityCase{
					name:           reserved.name + "/username/" + mapping.name,
					usernamePrefix: mapping.prefix, usernameClaim: mapping.claim, groupsClaims: []string{"developers"},
					wantUsername: reserved.value, wantGroups: []string{"developers"}, wantError: operatorUsernameValidationMessage,
				},
				identityCase{
					name:          reserved.name + "/group/" + mapping.name,
					usernameClaim: "alice", groupsPrefix: mapping.prefix, groupsClaims: []string{mapping.claim},
					wantUsername: "alice", wantGroups: []string{reserved.value}, wantError: operatorGroupsValidationMessage,
				},
			)
		}
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			provider := controllerconfig.OIDCProvider{
				IssuerURL: "https://issuer.example.com", Audiences: []string{"nke"},
				UsernameClaim: "sub", UsernamePrefix: tt.usernamePrefix,
				GroupsClaim: "groups", GroupsPrefix: tt.groupsPrefix,
			}
			config := mustBuildOperatorJWTAuthenticator(t, provider, policy)
			token := signOperatorJWT(t, signer, provider.IssuerURL, tt.usernameClaim, tt.groupsClaims)
			authenticator := newOperatorTokenAuthenticator(t, config, keySet)
			response, ok, err := authenticator.AuthenticateToken(ctx, token)
			if tt.wantError != "" {
				require.ErrorContains(t, err, "oidc: error evaluating user info validation rule")
				require.ErrorContains(t, err, tt.wantError)
				assert.False(t, ok)
				assert.Nil(t, response)

				// The same token must authenticate without the user rules. This proves
				// the rejection above follows signature verification and claim mapping.
				config.UserValidationRules = nil
				control := newOperatorTokenAuthenticator(t, config, keySet)
				response, ok, err = control.AuthenticateToken(ctx, token)
			}
			require.NoError(t, err)
			require.True(t, ok)
			require.NotNil(t, response)
			assert.Equal(t, tt.wantUsername, response.User.GetName())
			assert.Equal(t, tt.wantGroups, response.User.GetGroups())
		})
	}
}

// operatorJWTStaticKeySet verifies signatures with a key generated by the test.
// Providing it through Options.KeySet avoids discovery and JWKS network calls.
type operatorJWTStaticKeySet struct {
	publicKey *rsa.PublicKey
}

// VerifySignature checks the token's RS256 signature before returning its claims.
func (s operatorJWTStaticKeySet) VerifySignature(_ context.Context, token string) ([]byte, error) {
	signed, err := jose.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return nil, err
	}
	return signed.Verify(s.publicKey)
}

func newOperatorTokenAuthenticator(t *testing.T, config OIDCProvider, keySet operatorJWTStaticKeySet) kubeoidc.AuthenticatorTokenWithHealthCheck {
	t.Helper()

	var internalConfig apiserver.JWTAuthenticator
	require.NoError(t, apiserverv1.Convert_v1_JWTAuthenticator_To_apiserver_JWTAuthenticator(&config, &internalConfig, nil))
	authenticator, err := kubeoidc.New(t.Context(), kubeoidc.Options{JWTAuthenticator: internalConfig, KeySet: keySet})
	require.NoError(t, err)
	require.NoError(t, authenticator.HealthCheck())
	return authenticator
}

func signOperatorJWT(t *testing.T, signer jose.Signer, issuer, subject string, groups []string) string {
	t.Helper()

	payload, err := json.Marshal(map[string]any{
		"iss": issuer, "aud": []string{"nke"}, "exp": time.Now().Add(time.Hour).Unix(),
		"sub": subject, "groups": groups,
	})
	require.NoError(t, err)
	signed, err := signer.Sign(payload)
	require.NoError(t, err)
	token, err := signed.CompactSerialize()
	require.NoError(t, err)
	return token
}
