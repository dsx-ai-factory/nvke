// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/auth"
)

// rolesConfig varies the sole issuer's roles block, against a serviceAccounts
// admission rule so the structural Roles checks are exercised independent of
// which admission shape is in play.
func rolesConfig(roles string) string {
	return issuerConfig(`name: k8s
    issuer: "https://k8s.example.com"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit: {serviceAccounts: [{namespace: nke-clients}]}
    roles: ` + roles)
}

// rolesConfigSPIFFEAdmit is rolesConfig against a spiffe admission rule
// instead, so the SPIFFE-only Roles checks -- the ones that only run once the
// "requires admit.spiffe" gate has already passed -- can be exercised in
// isolation from it.
func rolesConfigSPIFFEAdmit(roles string) string {
	return issuerConfig(`name: spire
    issuer: "https://spire.example.com"
    jwks: {uri: "https://spire.example.com/keys"}
    algorithms: [RS256]
    admit: {spiffe: {trustDomain: example.org, pathPrefixes: ["/nke-service/"]}}
    roles: ` + roles)
}

// The structural Roles checks (design-authz-claim-based-roles.md, §3.1,
// §3.1a) -- everything Config.Validate refuses regardless of knownRoles, so
// runValidationCases' nil is the right knownRoles for all of them.
func TestLoadValidationRolesShape(t *testing.T) {
	t.Parallel()

	runValidationCases(t, []validationCase{
		{
			name:    "roles with none of claim, spiffe, serviceAccounts set",
			yaml:    rolesConfig(`{}`),
			wantErr: "must set exactly one of claim, spiffe, or serviceAccounts",
		},
		{
			name: "roles with both claim and spiffe set",
			yaml: rolesConfig(`
      claim: {name: groups, valueToRole: {eng: operator}}
      spiffe: [{expression: "path"}]`),
			wantErr: "must not set more than one",
		},
		{
			name: "roles with claim and an explicit empty serviceAccounts list",
			yaml: rolesConfig(`
      claim: {name: groups, valueToRole: {eng: operator}}
      serviceAccounts: []`),
			// Presence is nil-based, not length-based (design §3.1a's "Fix 2"): an
			// explicit empty list must still read as "set" for exclusivity, not be
			// silently reclassified as "unset" the way a length check would.
			wantErr: "must not set more than one",
		},
		{
			name:    "claim with no name",
			yaml:    rolesConfig(`{claim: {valueToRole: {eng: operator}}}`),
			wantErr: "claim.name is required",
		},
		{
			name:    "claim with an empty valueToRole map",
			yaml:    rolesConfig(`{claim: {name: groups, valueToRole: {}}}`),
			wantErr: "claim.valueToRole must contain at least one entry",
		},
		{
			name:    "claim with an empty-string key in valueToRole",
			yaml:    rolesConfig(`{claim: {name: groups, valueToRole: {"": operator}}}`),
			wantErr: "must not contain the empty string as a key",
		},
		{
			name:    "claim with an empty-string role value",
			yaml:    rolesConfig(`{claim: {name: groups, valueToRole: {eng: ""}}}`),
			wantErr: "is empty; every value must name a role",
		},
		{
			name:    "spiffe with an empty rule list",
			yaml:    rolesConfigSPIFFEAdmit(`{spiffe: []}`),
			wantErr: "spiffe must contain at least one rule",
		},
		{
			name:    "spiffe with an empty expression",
			yaml:    rolesConfigSPIFFEAdmit(`{spiffe: [{expression: ""}]}`),
			wantErr: "expression is required",
		},
		{
			name: "spiffe set without admit.spiffe on the same issuer",
			// rolesConfig's issuer admits by serviceAccounts, not spiffe.
			yaml:    rolesConfig(`{spiffe: [{expression: "path"}]}`),
			wantErr: "spiffe requires admit.spiffe on the same issuer",
		},
		{
			name:    "serviceAccounts with an empty rule list",
			yaml:    rolesConfig(`{serviceAccounts: []}`),
			wantErr: "serviceAccounts must contain at least one rule",
		},
		{
			name:    "serviceAccounts rule without a namespace",
			yaml:    rolesConfig(`{serviceAccounts: [{role: operator}]}`),
			wantErr: "namespace is required",
		},
		{
			name:    "serviceAccounts rule with an empty name",
			yaml:    rolesConfig(`{serviceAccounts: [{namespace: ci, name: "", role: operator}]}`),
			wantErr: "name is present but empty",
		},
		{
			name:    "serviceAccounts rule with no role",
			yaml:    rolesConfig(`{serviceAccounts: [{namespace: ci}]}`),
			wantErr: "role is required",
		},
	})
}

// knownRoleSet builds the map Config.Validate expects from
// authz.DeclaredRoleNames, without this package importing internal/authz.
func knownRoleSet(names ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return set
}

// baseIssuerWithRoles is a one-issuer Config whose Roles block a test fills
// in, admitting by service account so Roles.ServiceAccount cases don't also
// need Admit.SPIFFE (design §3.1a: no same-issuer Admit.ServiceAccounts
// requirement either, proven separately below).
func baseIssuerWithRoles(roles *auth.RoleMapping) *auth.Config {
	return &auth.Config{
		Audiences: []string{"https://cwe.example.com"},
		Issuers: []auth.Issuer{{
			Name:       "k8s",
			Issuer:     "https://k8s.example.com",
			JWKS:       &auth.JWKSKeys{URI: "https://kubernetes.default.svc/openid/v1/jwks"},
			Algorithms: []string{"RS256"},
			Admit: &auth.Admit{
				ServiceAccounts: []auth.ServiceAccountRule{{Namespace: "nke-clients"}},
			},
			Roles: roles,
		}},
	}
}

// The declared-role membership check (design §3.1's "Every Role value ...
// must correspond to a NKE::Role entity the loaded bundle actually
// declares", §7's knownRoles threading) -- needs a real, non-nil knownRoles,
// so these go directly through Config.Validate rather than runValidationCases.
func TestValidateRefusesAnUndeclaredRole(t *testing.T) {
	t.Parallel()

	known := knownRoleSet("operator", "reader")

	t.Run("claim.valueToRole value", func(t *testing.T) {
		t.Parallel()
		cfg := baseIssuerWithRoles(&auth.RoleMapping{
			Claim: &auth.ClaimRoleMapping{Name: "groups", ValueToRole: map[string]string{"eng": "typo-operator"}},
		})
		err := cfg.Validate(known)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"typo-operator"`)
		assert.Contains(t, err.Error(), "is not declared")
	})

	t.Run("serviceAccounts role", func(t *testing.T) {
		t.Parallel()
		cfg := baseIssuerWithRoles(&auth.RoleMapping{
			ServiceAccount: []auth.ServiceAccountRoleRule{{Namespace: "ci", Role: "typo-operator"}},
		})
		err := cfg.Validate(known)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"typo-operator"`)
		assert.Contains(t, err.Error(), "is not declared")
	})

	t.Run("a declared role passes", func(t *testing.T) {
		t.Parallel()
		cfg := baseIssuerWithRoles(&auth.RoleMapping{
			Claim: &auth.ClaimRoleMapping{Name: "groups", ValueToRole: map[string]string{"eng": "operator"}},
		})
		require.NoError(t, cfg.Validate(known))
	})
}

// nil knownRoles means authz is disabled for this deployment: the
// declared-role check specifically is skipped, but every other Roles
// constraint still runs and still fails startup (design §7).
func TestValidateWithNilKnownRolesSkipsOnlyTheDeclaredRoleCheck(t *testing.T) {
	t.Parallel()

	t.Run("an undeclared-looking role passes when authz is disabled", func(t *testing.T) {
		t.Parallel()
		cfg := baseIssuerWithRoles(&auth.RoleMapping{
			Claim: &auth.ClaimRoleMapping{Name: "groups", ValueToRole: map[string]string{"eng": "no-such-role"}},
		})
		require.NoError(t, cfg.Validate(nil), "a role authz will never check must not fail startup on a deployment that never turned authz on")
	})

	t.Run("every other Roles violation still fails with authz disabled", func(t *testing.T) {
		t.Parallel()
		cfg := baseIssuerWithRoles(&auth.RoleMapping{
			Claim: &auth.ClaimRoleMapping{Name: "", ValueToRole: map[string]string{"eng": "whatever"}},
		})
		require.Error(t, cfg.Validate(nil))
	})
}

// design §3.1a's corrected reasoning: Roles.ServiceAccount does not require
// Admit.ServiceAccounts on the same issuer. The runtime subject-consistency
// check (roles.go, serviceAccountRolesFor) is what makes that safe, not a
// same-issuer config requirement -- see TestServiceAccountRolesForRequiresSubjectConsistency
// in roles_test.go for the runtime half of this proof.
func TestValidateAllowsServiceAccountRolesOnASPIFFEIssuer(t *testing.T) {
	t.Parallel()

	cfg := &auth.Config{
		Audiences: []string{"https://cwe.example.com"},
		Issuers: []auth.Issuer{{
			Name:       "spire",
			Issuer:     "https://spire.example.com",
			JWKS:       &auth.JWKSKeys{URI: "https://spire.example.com/keys"},
			Algorithms: []string{"RS256"},
			Admit: &auth.Admit{
				SPIFFE: &auth.SPIFFERule{TrustDomain: "example.org", PathPrefixes: []string{"/nke-service/"}},
			},
			Roles: &auth.RoleMapping{
				ServiceAccount: []auth.ServiceAccountRoleRule{{Namespace: "ci", Role: "operator"}},
			},
		}},
	}
	require.NoError(t, cfg.Validate(knownRoleSet("operator")))
}
