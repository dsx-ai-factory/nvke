// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authn_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"
)

const tenantClaim = "dsx.nvidia.com/tenant"

// tenantSourceConfig varies the sole issuer's tenant source, as raw YAML.
func tenantSourceConfig(source string) string {
	return issuerConfig(`name: k8s
    issuer: "https://k8s.example.com"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}
    tenant: ` + source)
}

func TestLoadValidationTenant(t *testing.T) {
	t.Parallel()

	runValidationCases(t, []validationCase{
		{name: "a claim source loads", yaml: tenantSourceConfig(`{claim: org_id}`)},
		{name: "a fixed source loads", yaml: tenantSourceConfig(`{fixed: acme}`)},
		{
			name:    "both claim and fixed",
			yaml:    tenantSourceConfig(`{claim: org_id, fixed: acme}`),
			wantErr: "tenant: must set only one of claim and fixed",
		},
		{
			name:    "an empty tenant block",
			yaml:    tenantSourceConfig(`{}`),
			wantErr: "tenant: must set claim or fixed",
		},
		{
			name:    "a padded claim name",
			yaml:    tenantSourceConfig(`{claim: " org_id"}`),
			wantErr: `claim " org_id" has leading or trailing whitespace`,
		},
		{
			name:    "a padded fixed tenant",
			yaml:    tenantSourceConfig(`{fixed: "acme "}`),
			wantErr: `fixed "acme " has leading or trailing whitespace`,
		},
		{
			name:    "a fixed tenant with a control character",
			yaml:    tenantSourceConfig(`{fixed: "ac\tme"}`),
			wantErr: `tenant: fixed "ac\tme" holds a character a tenant ID cannot contain`,
		},
		{
			name:    "a fixed tenant with a quote",
			yaml:    tenantSourceConfig(`{fixed: 'ac"me'}`),
			wantErr: "holds a character a tenant ID cannot contain",
		},
	})
}

// YAML can't carry invalid UTF-8, so this case builds the config in Go.
func TestValidateRefusesAFixedTenantThatIsNotUTF8(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.Issuers[0].Tenant = &authn.TenantSource{Fixed: "ac\xffme"}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not valid UTF-8")
}

// claimTenant reads the SPIRE issuer's tenant from tenantClaim; the Kubernetes
// issuer has no tenant.
func claimTenant(cfg *authn.Config) {
	cfg.Issuers[1].Tenant = &authn.TenantSource{Claim: tenantClaim}
}

func spireToken(t *testing.T, f fixture, claims map[string]any) string {
	t.Helper()
	spec := validSPIREToken(f, time.Now())
	spec.Extra = claims
	return MintToken(t, spec)
}

// A claim yields a tenant only as a string that is a valid tenant ID. Anything
// else leaves the caller authenticated with no tenant.
func TestTenantFromClaim(t *testing.T) {
	t.Parallel()

	atLimit := strings.Repeat("a", 256)
	tests := []struct {
		name   string
		claims map[string]any
		want   string
	}{
		{name: "a string", claims: map[string]any{tenantClaim: "acme"}, want: "acme"},
		{name: "a string with an inner space", claims: map[string]any{tenantClaim: "ac me"}, want: "ac me"},
		{name: "exactly 256 bytes", claims: map[string]any{tenantClaim: atLimit}, want: atLimit},
		{name: "257 bytes", claims: map[string]any{tenantClaim: atLimit + "a"}, want: ""},
		{name: "absent", claims: nil, want: ""},
		{name: "a one-element list", claims: map[string]any{tenantClaim: []any{"acme"}}, want: ""},
		{name: "an object", claims: map[string]any{tenantClaim: map[string]any{"acme": map[string]any{}}}, want: ""},
		{name: "a number", claims: map[string]any{tenantClaim: 42}, want: ""},
		{name: "empty", claims: map[string]any{tenantClaim: ""}, want: ""},
		{name: "padded", claims: map[string]any{tenantClaim: " acme"}, want: ""},
		{name: "a newline", claims: map[string]any{tenantClaim: "ac\nme"}, want: ""},
		{name: "DEL", claims: map[string]any{tenantClaim: "ac\x7fme"}, want: ""},
		{name: "a C1 control", claims: map[string]any{tenantClaim: "ac\u0085me"}, want: ""},
		{name: "a zero-width space", claims: map[string]any{tenantClaim: "\u200bacme"}, want: ""},
		{name: "a bidi override", claims: map[string]any{tenantClaim: "ac\u202eme"}, want: ""},
		{name: "a line separator", claims: map[string]any{tenantClaim: "ac\u2028me"}, want: ""},
		{name: "a replacement character", claims: map[string]any{tenantClaim: "ac\ufffdme"}, want: ""},
		// json.Marshal writes the invalid byte as \ufffd, as a signer does for a
		// claim that isn't UTF-8.
		{name: "invalid UTF-8", claims: map[string]any{tenantClaim: "ac\xffme"}, want: ""},
		{name: "a quote", claims: map[string]any{tenantClaim: `ac"me`}, want: ""},
		{name: "a backslash", claims: map[string]any{tenantClaim: `ac\me`}, want: ""},
		{
			name:   "the claim name is one key, not a path",
			claims: map[string]any{"dsx.nvidia.com": map[string]any{"/tenant": "acme"}},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFixtureWith(t, claimTenant)
			p, err := f.verifier.Authenticate(context.Background(), spireToken(t, f, tt.claims))
			require.NoError(t, err, "a tenant problem never fails authentication")
			assert.Equal(t, tt.want, p.Tenant)
		})
	}
}

func TestTenantAbsentWithoutASource(t *testing.T) {
	t.Parallel()

	f := newFixtureWith(t, claimTenant)
	p, err := f.verifier.Authenticate(context.Background(), MintToken(t, validKubernetesToken(f, time.Now())))
	require.NoError(t, err)
	assert.Empty(t, p.Tenant)
}

func TestFixedTenant(t *testing.T) {
	t.Parallel()

	f := newFixtureWith(t, func(cfg *authn.Config) {
		cfg.Issuers[0].Tenant = &authn.TenantSource{Fixed: "acme"}
	})
	p, err := f.verifier.Authenticate(context.Background(), MintToken(t, validKubernetesToken(f, time.Now())))
	require.NoError(t, err)
	assert.Equal(t, "acme", p.Tenant)
}
