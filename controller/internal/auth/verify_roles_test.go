// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/auth"
)

// The integration half of design-authz-claim-based-roles.md's §3.2: role
// derivation lives in Authenticate, wired end to end through a real Verifier
// rather than the roles.go unit tests' direct calls to rolesFor.

// testConfigServiceAccountRoles is testConfig's Kubernetes issuer, with a
// Roles.ServiceAccount block granting "operator" to the same namespace its
// Admit already admits.
func testConfigServiceAccountRoles() *auth.Config {
	cfg := testConfig()
	for i := range cfg.Issuers {
		if cfg.Issuers[i].Name == k8sName {
			cfg.Issuers[i].Roles = &auth.RoleMapping{
				ServiceAccount: []auth.ServiceAccountRoleRule{
					{Namespace: admittedNamespace, Role: "operator"},
				},
			}
		}
	}
	return cfg
}

// testConfigSPIFFEServiceAccountRoles is testConfig's SPIRE issuer, with the
// identical Roles.ServiceAccount block -- admitting via SPIFFE, not
// serviceAccounts, so a token can be admitted while still carrying an
// independent "kubernetes.io" claim (design §3.1a's "Subject-consistency
// enforcement" gap).
func testConfigSPIFFEServiceAccountRoles() *auth.Config {
	cfg := testConfig()
	for i := range cfg.Issuers {
		if cfg.Issuers[i].Name == spireName {
			cfg.Issuers[i].Roles = &auth.RoleMapping{
				ServiceAccount: []auth.ServiceAccountRoleRule{
					{Namespace: admittedNamespace, Role: "operator"},
				},
			}
		}
	}
	return cfg
}

func newRolesFixture(t *testing.T, cfg *auth.Config) fixture {
	t.Helper()

	signing := RSAKey(t, "roles-signing-1")
	elliptic := ECDSAKey(t, "roles-elliptic-1")
	rogue := RSAKey(t, "roles-rogue-1")

	k8sKeys := NewStaticKeySource(signing, elliptic)
	spireKeys := NewStaticKeySource(signing, elliptic)

	verifier, err := auth.NewVerifier(cfg, map[string]auth.KeySource{
		k8sName:   k8sKeys,
		spireName: spireKeys,
	})
	require.NoError(t, err)

	return fixture{verifier: verifier, signing: signing, elliptic: elliptic, rogue: rogue, k8sKeys: k8sKeys}
}

// Authenticate populates ClaimRoles for an admitted principal, on top of
// whatever roles.json would separately grant it (which this test, calling
// Authenticate directly, never touches -- that union happens in
// internal/authz, see local_roles_test.go).
func TestAuthenticateDerivesClaimRolesForAnAdmittedPrincipal(t *testing.T) {
	t.Parallel()

	f := newRolesFixture(t, testConfigServiceAccountRoles())
	spec := validKubernetesToken(f, time.Now())

	p, err := f.verifier.Authenticate(context.Background(), MintToken(t, spec))
	require.NoError(t, err)
	assert.Equal(t, []string{"operator"}, p.ClaimRoles)
}

// A principal Admit refuses never reaches role derivation -- Authenticate
// returns the admission error, not a Principal with roles computed and
// discarded (design §3.2's ordering rationale).
func TestAuthenticateNeverDerivesRolesForAPrincipalAdmitRefuses(t *testing.T) {
	t.Parallel()

	f := newRolesFixture(t, testConfigServiceAccountRoles())
	spec := validKubernetesToken(f, time.Now())
	// "default" is outside the admitted namespace (admittedNamespace, see
	// testConfig), so Admit refuses this principal before rolesFor ever runs.
	spec.Subject = "system:serviceaccount:default:runner"
	spec.Extra = ServiceAccountClaims("default", "runner")

	p, err := f.verifier.Authenticate(context.Background(), MintToken(t, spec))
	require.Error(t, err)
	assert.Nil(t, p)
	reason, ok := auth.ReasonOf(err)
	require.True(t, ok)
	assert.Equal(t, auth.ReasonNotAdmitted, reason)
}

// The direct, end-to-end proof of the gap both design-authz-claim-based-roles.md
// review rounds found and §3.1a's "Subject-consistency enforcement" fixes: a
// principal admitted via the SPIFFE branch (which never inspects
// Principal.Claims) that also happens to carry a well-formed but unrelated
// "kubernetes.io" claim must not have that claim trusted for a role grant.
func TestAuthenticateServiceAccountRolesRequireSubjectConsistency(t *testing.T) {
	t.Parallel()

	f := newRolesFixture(t, testConfigSPIFFEServiceAccountRoles())
	spec := validSPIREToken(f, time.Now())
	// A claim naming a service account this issuer's Roles config would
	// otherwise grant "operator" for -- but Subject is the SPIFFE ID above, not
	// system:serviceaccount:<admittedNamespace>:<admittedName>.
	spec.Extra = ServiceAccountClaims(admittedNamespace, admittedName)

	p, err := f.verifier.Authenticate(context.Background(), MintToken(t, spec))
	require.NoError(t, err, "admission is unaffected: the SPIFFE branch never looks at this claim")
	assert.Empty(t, p.ClaimRoles, "a kubernetes.io claim inconsistent with the verified subject must not grant a role")
}
