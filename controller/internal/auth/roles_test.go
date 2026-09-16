// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"testing"

	"cel.dev/cel-go/cel"
	"github.com/golang-jwt/jwt/v5"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serviceAccountClaimsForRoles is this file's own copy of fixtures_test.go's
// ServiceAccountClaims: that one lives in package auth_test and this file is
// white-box package auth (see the file-level comment), so it cannot be
// shared across the package boundary.
func serviceAccountClaimsForRoles(namespace, name string) map[string]any {
	return map[string]any{
		"kubernetes.io": map[string]any{
			"namespace": namespace,
			"serviceaccount": map[string]any{
				"name": name,
			},
		},
	}
}

// White-box: rolesFor and its helpers are unexported (design
// -authz-claim-based-roles.md, §3.2 -- role resolution lives beside
// admitRules.admits, not behind an exported seam), so this file is package
// auth rather than auth_test. keys_test.go and routes_escaping_test.go are
// this package's existing precedent for that.

func mustResolveClaim(t *testing.T, m *RoleMapping) *roleRules {
	t.Helper()
	rules, err := m.resolve(&admitRules{})
	require.NoError(t, err)
	return rules
}

// claimRolesFor: the OIDC/claim matching table from design §5 and §6.
func TestClaimRolesFor(t *testing.T) {
	t.Parallel()

	rules := &claimRoleRules{
		name: "groups",
		valueToRole: map[string]string{
			"eng-a": "operator",
			"eng-b": "reader",
		},
	}

	tests := []struct {
		name   string
		claims jwt.MapClaims
		want   []string
	}{
		{"claim absent", jwt.MapClaims{}, nil},
		{"a matched string claim", jwt.MapClaims{"groups": "eng-a"}, []string{"operator"}},
		{"a present but unmapped string claim", jwt.MapClaims{"groups": "eng-nobody-mapped"}, nil},
		{
			"a matched array claim, Entra's shape",
			jwt.MapClaims{"groups": []any{"eng-a", "eng-b"}},
			[]string{"operator", "reader"},
		},
		{
			"an array claim with one matched and one unmapped value",
			jwt.MapClaims{"groups": []any{"eng-a", "not-mapped"}},
			[]string{"operator"},
		},
		{"the claim value is the wrong JSON type: a number", jwt.MapClaims{"groups": float64(7)}, nil},
		{"the claim value is the wrong JSON type: an object", jwt.MapClaims{"groups": map[string]any{}}, nil},
		{"the claim value is the wrong JSON type: a bool", jwt.MapClaims{"groups": true}, nil},
		{
			// The whole claim contributes zero roles, not just the bad element --
			// asserted against a ValueToRole where "eng-a" IS a valid key, so this
			// distinguishes "the whole claim was rejected" from "the one good
			// element still matched" (design §5, §6).
			"a mixed-type array claim contributes nothing, not a partial match",
			jwt.MapClaims{"groups": []any{"eng-a", 7}},
			nil,
		},
		{
			// A claim value that is textually a role name but not a configured key
			// grants nothing -- claim values are only ever map lookup keys, never
			// rendered into an entity UID (design §6).
			"a claim value textually equal to a role name, but not a configured key",
			jwt.MapClaims{"groups": "operator"},
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, claimRolesFor(rules, tt.claims))
		})
	}
}

// rolesFor's claim branch, exercised through the same *RoleMapping.resolve
// path NewVerifier uses, so this also proves resolve wires claimRoleRules
// correctly rather than only that claimRolesFor itself works.
func TestRolesForClaimBranch(t *testing.T) {
	t.Parallel()

	m := &RoleMapping{Claim: &ClaimRoleMapping{
		Name:        "groups",
		ValueToRole: map[string]string{"eng-a": "operator"},
	}}
	rules := mustResolveClaim(t, m)

	got := rules.rolesFor(jwt.MapClaims{"groups": []any{"eng-a"}}, "irrelevant-for-claims")
	assert.Equal(t, []string{"operator"}, got)
}

// A nil *roleRules -- an issuer with no Roles config -- contributes nothing,
// and Authenticate calls rolesFor on it unconditionally (verify.go).
func TestRolesForOnANilReceiverContributesNothing(t *testing.T) {
	t.Parallel()

	var rules *roleRules
	assert.Nil(t, rules.rolesFor(jwt.MapClaims{"groups": "eng-a"}, "subject"))
}

// serviceAccountRolesFor: the PSAT table from design §5's addendum and §6's
// PSAT mutation-style tests.
func TestServiceAccountRolesFor(t *testing.T) {
	t.Parallel()

	name := "deploy-bot"
	namespaced := []serviceAccountRoleRules{
		{namespace: "ci", name: &name, role: "deployer"},
		{namespace: "ci", role: "ci-reader"}, // AllowsAnyName: nil name
	}

	claims := serviceAccountClaimsForRoles("ci", "deploy-bot")

	t.Run("matching grant, subject consistent", func(t *testing.T) {
		t.Parallel()
		got := serviceAccountRolesFor(namespaced, claims, "system:serviceaccount:ci:deploy-bot")
		// Both the name-specific and the namespace-wide rule match: overlap
		// unions, it does not pick one (design §3.1a, §5).
		assert.ElementsMatch(t, []string{"deployer", "ci-reader"}, got)
	})

	t.Run("subject inconsistent: a SPIFFE sub grants nothing", func(t *testing.T) {
		t.Parallel()
		got := serviceAccountRolesFor(namespaced, claims, "spiffe://example.org/nke-service")
		assert.Nil(t, got, "a token admitted on a different identity must not have its kubernetes.io claim trusted for a role grant")
	})

	t.Run("subject inconsistent: a different service account's canonical subject grants nothing", func(t *testing.T) {
		t.Parallel()
		got := serviceAccountRolesFor(namespaced, claims, "system:serviceaccount:ci:someone-else")
		assert.Nil(t, got)
	})

	t.Run("namespace matches, name does not: no partial credit", func(t *testing.T) {
		t.Parallel()
		rules := []serviceAccountRoleRules{{namespace: "ci", name: &name, role: "deployer"}}
		other := serviceAccountClaimsForRoles("ci", "other-sa")
		got := serviceAccountRolesFor(rules, other, "system:serviceaccount:ci:other-sa")
		assert.Nil(t, got)
	})

	t.Run("AllowsAnyName matches every name in its namespace", func(t *testing.T) {
		t.Parallel()
		rules := []serviceAccountRoleRules{{namespace: "ci", role: "ci-reader"}}
		for _, sa := range []string{"deploy-bot", "anything-else"} {
			c := serviceAccountClaimsForRoles("ci", sa)
			got := serviceAccountRolesFor(rules, c, "system:serviceaccount:ci:"+sa)
			assert.Equal(t, []string{"ci-reader"}, got)
		}
	})

	t.Run("AllowsAnyName does not match outside its namespace", func(t *testing.T) {
		t.Parallel()
		rules := []serviceAccountRoleRules{{namespace: "ci", role: "ci-reader"}}
		c := serviceAccountClaimsForRoles("default", "deploy-bot")
		got := serviceAccountRolesFor(rules, c, "system:serviceaccount:default:deploy-bot")
		assert.Nil(t, got)
	})

	fieldCases := []struct {
		name   string
		claims map[string]any
	}{
		{"kubernetes.io absent", map[string]any{}},
		{"kubernetes.io not a map", map[string]any{"kubernetes.io": "nope"}},
		{"namespace absent", map[string]any{"kubernetes.io": map[string]any{
			"serviceaccount": map[string]any{"name": "deploy-bot"},
		}}},
		{"serviceaccount absent", map[string]any{"kubernetes.io": map[string]any{"namespace": "ci"}}},
		{"serviceaccount.name absent", map[string]any{"kubernetes.io": map[string]any{
			"namespace": "ci", "serviceaccount": map[string]any{},
		}}},
	}
	for _, tt := range fieldCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.NotPanics(t, func() {
				got := serviceAccountRolesFor(namespaced, tt.claims, "system:serviceaccount:ci:deploy-bot")
				assert.Nil(t, got)
			})
		})
	}
}

// rolesFor's serviceAccount branch, through *RoleMapping.resolve, proving
// resolve wires serviceAccountRoleRules correctly and that the branch is
// reachable from the same call Authenticate makes.
func TestRolesForServiceAccountBranch(t *testing.T) {
	t.Parallel()

	m := &RoleMapping{ServiceAccount: []ServiceAccountRoleRule{
		{Namespace: "ci", Role: "deployer"},
	}}
	rules := mustResolveClaim(t, m)

	claims := serviceAccountClaimsForRoles("ci", "deploy-bot")
	got := rules.rolesFor(claims, "system:serviceaccount:ci:deploy-bot")
	assert.Equal(t, []string{"deployer"}, got)

	// Subject-consistency gate reachable through the same public entry point
	// Authenticate uses.
	got = rules.rolesFor(claims, "spiffe://example.org/nke-service")
	assert.Nil(t, got)
}

// spiffeAdmitRules builds an *admitRules carrying only the parsed
// "example.org" trust domain rolesFor's SPIFFE branch needs -- pathPrefixes
// are Admit's own concern (admit.go), irrelevant to role derivation, which is
// why they're left unset here. Every test below shares one trust domain
// (admit_test.go's own adversarial cases use the same one), so this takes no
// parameter rather than one every caller would pass identically.
func spiffeAdmitRules(t *testing.T) *admitRules {
	t.Helper()
	td, err := spiffeid.TrustDomainFromString("example.org")
	require.NoError(t, err)
	return &admitRules{spiffe: &spiffeRules{trustDomain: td}}
}

func mustResolveSPIFFE(t *testing.T, admit *admitRules, exprs ...string) *roleRules {
	t.Helper()
	rules := make([]SPIFFERoleRule, len(exprs))
	for i, e := range exprs {
		rules[i] = SPIFFERoleRule{Expression: e}
	}
	m := &RoleMapping{SPIFFE: rules}
	got, err := m.resolve(admit)
	require.NoError(t, err)
	return got
}

// design §3.4's two worked examples, proving the actual point of the CEL
// mechanism: two genuinely different SPIFFE path topologies, the same
// evaluation environment, no Go code change between them -- only a different
// Expression string.
const (
	// Deployment A: NKE's own current shape, role is a leading path segment.
	spiffeExprLeadingRoleSegment = `path.startsWith("/role/") ? path.split("/")[2] : ""`
	// Deployment B: a different customer's convention, role is a trailing
	// segment.
	spiffeExprTrailingRoleSegment = `path.split("/").size() >= 2 && ` +
		`path.split("/")[path.split("/").size() - 2] == "role" ` +
		`? path.split("/")[path.split("/").size() - 1] : ""`
)

func TestRolesForSPIFFEBranchWorkedExamples(t *testing.T) {
	t.Parallel()

	admit := spiffeAdmitRules(t)

	t.Run("deployment A: leading /role/<name> segment", func(t *testing.T) {
		t.Parallel()
		rules := mustResolveSPIFFE(t, admit, spiffeExprLeadingRoleSegment)

		got := rules.rolesFor(jwt.MapClaims{}, "spiffe://example.org/role/operator/nke-service/cwe")
		assert.Equal(t, []string{"operator"}, got)

		got = rules.rolesFor(jwt.MapClaims{}, "spiffe://example.org/nke-service/cloud-workflow-engine")
		assert.Nil(t, got, "a path that doesn't match this deployment's convention grants nothing")
	})

	t.Run("deployment B: trailing .../role/<name> segment", func(t *testing.T) {
		t.Parallel()
		rules := mustResolveSPIFFE(t, admit, spiffeExprTrailingRoleSegment)

		got := rules.rolesFor(jwt.MapClaims{}, "spiffe://example.org/bm/host-1/process/nke-agent/role/operator")
		assert.Equal(t, []string{"operator"}, got)

		got = rules.rolesFor(jwt.MapClaims{}, "spiffe://example.org/bm/host-1/process/nke-agent")
		assert.Nil(t, got, "a path that doesn't match this deployment's convention grants nothing")
	})

	t.Run("swapping Expression strings serves a second topology with no Go code change", func(t *testing.T) {
		t.Parallel()
		// The same rules value type, the same rolesFor call -- only the
		// configured Expression differs between deployment A and B.
		a := mustResolveSPIFFE(t, admit, spiffeExprLeadingRoleSegment)
		b := mustResolveSPIFFE(t, admit, spiffeExprTrailingRoleSegment)
		path := "spiffe://example.org/bm/host-1/process/nke-agent/role/reader"
		assert.Nil(t, a.rolesFor(jwt.MapClaims{}, path), "deployment A's rule doesn't understand deployment B's shape")
		assert.Equal(t, []string{"reader"}, b.rolesFor(jwt.MapClaims{}, path))
	})
}

// A deployment needing more than one SPIFFE-derived role writes more than one
// SPIFFERoleRule; rolesFor unions whatever non-empty strings its configured
// programs produce (design §3.4's "why one expression produces one role
// name, not a list").
func TestRolesForSPIFFEBranchUnionsMultipleRules(t *testing.T) {
	t.Parallel()

	admit := spiffeAdmitRules(t)
	rules := mustResolveSPIFFE(t, admit,
		`path.startsWith("/role/") ? path.split("/")[2] : ""`,
		`path.startsWith("/env/") ? "env-"+path.split("/")[2] : ""`,
	)

	got := rules.rolesFor(jwt.MapClaims{}, "spiffe://example.org/role/operator")
	assert.Equal(t, []string{"operator"}, got)

	got = rules.rolesFor(jwt.MapClaims{}, "spiffe://example.org/env/staging")
	assert.Equal(t, []string{"env-staging"}, got)

	// Neither expression matches this path: zero roles, not a partial or
	// erroneous result.
	got = rules.rolesFor(jwt.MapClaims{}, "spiffe://example.org/other")
	assert.Nil(t, got)
}

// The structural trust-domain check runs before any CEL program does, and
// must never disagree with Admits' own adversarial trust-domain cases
// (admit_test.go): a subject outside the issuer's trust domain contributes no
// role, the same as it isn't admitted at all.
func TestRolesForSPIFFEBranchTrustDomainIsStructuralNotCEL(t *testing.T) {
	t.Parallel()

	admit := spiffeAdmitRules(t)
	// A rule that would match "/role/operator" if it ever ran.
	rules := mustResolveSPIFFE(t, admit, spiffeExprLeadingRoleSegment)

	tests := []struct {
		name    string
		subject string
		want    []string
	}{
		{"member of the trust domain", "spiffe://example.org/role/operator", []string{"operator"}},
		{
			// The exact adversarial case admit_test.go's spiffePrincipal cases
			// cover for admission: a suffix match must not be accepted as
			// membership.
			"a suffix-matching but distinct trust domain",
			"spiffe://example.org.attacker.test/role/operator",
			nil,
		},
		{"not a SPIFFE ID at all", "not-a-spiffe-id", nil},
		{"empty subject", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, rules.rolesFor(jwt.MapClaims{}, tt.subject))
		})
	}
}

// A CEL expression that errors at evaluation time -- here, an out-of-range
// split index for a path shorter than the expression assumes -- contributes
// no role, not a crash and not a Go error propagated out of rolesFor (design
// §3.4, §5, §6).
func TestRolesForSPIFFEBranchEvaluationErrorContributesNothing(t *testing.T) {
	t.Parallel()

	admit := spiffeAdmitRules(t)
	// path.split("/")[2] assumes at least three segments; a path with fewer
	// makes this an out-of-range index at evaluation time, not compile time.
	rules := mustResolveSPIFFE(t, admit, `path.split("/")[2]`)

	assert.NotPanics(t, func() {
		got := rules.rolesFor(jwt.MapClaims{}, "spiffe://example.org/short")
		assert.Nil(t, got)
	})
}

// evalSPIFFERoleExpression's non-string branch is defense in depth --
// Config.Validate's ast.OutputType() check already refuses any Expression
// that doesn't statically type-check to string, so a well-formed
// SPIFFERoleRule can never actually reach this at runtime. Exercised
// directly against the helper, not through a config that could pass
// Validate, since CEL's static typing makes a non-string *runtime* result
// from a Validate-accepted expression not constructible.
func TestEvalSPIFFERoleExpressionRejectsNonStringResults(t *testing.T) {
	t.Parallel()

	env, err := spiffeCELEnv()
	require.NoError(t, err)

	ast, issues := env.Compile(`path.size()`) // type-checks to int, not string
	require.NoError(t, issues.Err())
	require.NotEqual(t, cel.StringType, ast.OutputType())

	program, err := env.Program(ast)
	require.NoError(t, err)

	role, ok := evalSPIFFERoleExpression(program, "/role/operator")
	assert.False(t, ok)
	assert.Empty(t, role)
}

// ext.Strings() is not part of CEL's core/default environment -- .split(...)
// specifically requires it, which is why spiffeCELEnv enables it (design
// §3.4). Proven here against a bare environment, distinct from
// spiffeCELEnv's own production environment, which always enables it.
func TestSPIFFESplitRequiresExtStrings(t *testing.T) {
	t.Parallel()

	bare, err := cel.NewEnv(cel.Variable(spiffeCELPathVar, cel.StringType))
	require.NoError(t, err)

	_, issues := bare.Compile(`path.split("/")[0]`)
	require.Error(t, issues.Err())
	assert.Contains(t, issues.Err().Error(), "split")
}

// matchesServiceAccount and validateServiceAccountSelector are shared between
// admission (admit.go) and role derivation (roles.go) so the two cannot
// drift about what "the same service account" means (design §3.1a's "Shared
// validation and matching" bullet).
func TestMatchesServiceAccount(t *testing.T) {
	t.Parallel()

	sa := serviceAccountIdentity{namespace: "ci", name: "deploy-bot"}
	name := "deploy-bot"
	other := "someone-else"

	assert.True(t, matchesServiceAccount("ci", nil, sa), "a nil name matches any name in the namespace")
	assert.True(t, matchesServiceAccount("ci", &name, sa))
	assert.False(t, matchesServiceAccount("ci", &other, sa))
	assert.False(t, matchesServiceAccount("default", nil, sa))
}

func TestValidateServiceAccountSelector(t *testing.T) {
	t.Parallel()

	name := "deploy-bot"
	empty := ""

	require.NoError(t, validateServiceAccountSelector("ci", nil))
	require.NoError(t, validateServiceAccountSelector("ci", &name))
	require.Error(t, validateServiceAccountSelector("", nil), "namespace required")
	require.Error(t, validateServiceAccountSelector("ci", &empty), "present-but-empty name refused")
	require.Error(t, validateServiceAccountSelector("Not_A_Namespace", nil), "not a valid DNS label")
}
