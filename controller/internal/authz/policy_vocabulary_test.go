// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Entity-instance checks, which schema validation does not do.
//
// policy_validation_test.go type-checks the bundle against schema.json: it
// catches an action, an entity type, an attribute or a context key that the
// schema does not declare. What it cannot catch is an entity *instance* that
// does not exist, because Cedar validation is type-based — NKE::Role::"raeder"
// is a perfectly well-typed reference to a role nobody declared, and a policy
// naming it validates clean and then matches nothing. Verified: the validator
// passes it.
//
// So these two layers are complementary and neither subsumes the other:
//
//	the validator   types — is this attribute declared, of the right type?
//	these tests     instances — does this role exist, does anyone hold it?
//
// The checks themselves moved to validate.go (checkActionVocabulary,
// checkRoleVocabulary, checkRolesAreHeld, checkRoleReachability) as part of
// design-authz-chart-validation.md §6: NewLocal now enforces three of the
// four at load (checkRolesAreHeld is the one exception -- see
// ValidateBundle's doc comment in validate.go for why), and these tests are
// now thin wrappers asserting the shipped bundle at deploy/authz satisfies
// each of the four, rather than a second copy of the logic.

func TestEveryActionAPolicyNamesExists(t *testing.T) {
	dir := bundleDir(t)

	source, err := os.ReadFile(filepath.Join(dir, policiesFile))
	require.NoError(t, err)
	require.Regexp(t, actionRef, string(source), "no policy names an action; the check would pass vacuously")

	errs := checkActionVocabulary(dir, source)
	assert.Empty(t, errs, "the shipped bundle must name only actions its own actions.json declares: %v", errs)
}

func TestEveryRoleAPolicyNamesExists(t *testing.T) {
	dir := bundleDir(t)

	source, err := os.ReadFile(filepath.Join(dir, policiesFile))
	require.NoError(t, err)
	require.Regexp(t, roleRef, string(source), "no policy names a role; the check would pass vacuously")

	errs := checkRoleVocabulary(dir, source)
	assert.Empty(t, errs, "the shipped bundle must name only roles its own roles.json declares: %v", errs)
}

// A role nobody holds grants nothing, which is usually a mistake rather than an
// intention — and it is invisible, because the policy naming it still loads.
//
// checkRolesAreHeld is not part of ValidateBundle/NewLocal (validate.go's own
// doc comment explains why: it does not generalize to a claim-derived role),
// so this test calls it directly, against the shipped bundle only, where
// every role happens to still be held statically today.
func TestEveryRoleIsHeldBySomeone(t *testing.T) {
	errs := checkRolesAreHeld(bundleDir(t))
	assert.Empty(t, errs, "the shipped bundle must not declare a role nobody holds: %v", errs)
}

// The other direction, which the two checks above leave open: a role that is
// declared, and held, and that no policy ever names.
func TestEveryDeclaredRoleIsNamedByAPolicy(t *testing.T) {
	dir := bundleDir(t)

	source, err := os.ReadFile(filepath.Join(dir, policiesFile))
	require.NoError(t, err)
	require.Regexp(t, roleRef, string(source), "no policy names a role; the check would pass vacuously")

	errs := checkRoleReachability(dir, source)
	assert.Empty(t, errs, "the shipped bundle must not declare a role no policy names, directly or through nesting: %v", errs)
}

// The mutation-style proof that roleIsNamed's direction is the right one: a
// role planted with no policy naming it, and no ancestor a policy names,
// against a hierarchy that does contain nesting — so a reachability check that
// (wrongly) looked at descendants instead of ancestors would pass this role by
// mistake, the way it passed the real bundle by accident: every role the
// shipped bundle declares is also named directly, so the buggy direction was
// never exercised there.
func TestRoleIsNamedReachesThroughAncestorsNotDescendants(t *testing.T) {
	// reader <- operator <- superOperator (superOperator is nested under
	// operator, operator is nested under reader).
	parents := map[string]map[string]bool{
		"operator":      {"reader": true},
		"superOperator": {"operator": true},
		"orphan":        {},
	}

	// Only "operator" is named directly.
	named := map[string]bool{"operator": true}

	assert.True(t, roleIsNamed("operator", named, parents), "operator is named directly")
	assert.Truef(t, roleIsNamed("superOperator", named, parents),
		"superOperator is nested under operator, which is named, so a principal holding superOperator is also \"in\" operator")
	assert.Falsef(t, roleIsNamed("reader", named, parents),
		"reader is an ancestor of the named role, not a descendant of it — naming operator does not name reader, "+
			"and a check that thinks it does would miss a genuinely unused role")
	assert.Falsef(t, roleIsNamed("orphan", named, parents),
		"orphan has no ancestor a policy names, so it grants nothing and the check must reject it")
}
