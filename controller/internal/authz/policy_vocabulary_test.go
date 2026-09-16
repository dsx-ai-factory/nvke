// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
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
// The action check overlaps the validator deliberately, because it also asserts
// the generated actions.json agrees with the generated operations table; a stale
// entity file would otherwise make a real action look absent at runtime while
// the schema still declared it.

var (
	actionRef = regexp.MustCompile(`NKE::Action::"([^"]+)"`)
	roleRef   = regexp.MustCompile(`NKE::Role::"([^"]+)"`)
)

// entityIDs reads one entity file and returns the ids of the given type.
func entityIDs(t *testing.T, path, entityType string) map[string]bool {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var entities []struct {
		UID struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"uid"`
	}
	require.NoError(t, json.Unmarshal(raw, &entities))

	ids := map[string]bool{}
	for _, e := range entities {
		if e.UID.Type == entityType {
			ids[e.UID.ID] = true
		}
	}
	return ids
}

func TestEveryActionAPolicyNamesExists(t *testing.T) {
	dir := bundleDir(t)

	source, err := os.ReadFile(filepath.Join(dir, policiesFile))
	require.NoError(t, err)

	// Actions come from two places: the generated per-operation entities, and
	// the coarse groups they are members of.
	known := entityIDs(t, filepath.Join(dir, actionsFile), "NKE::Action")
	require.NotEmpty(t, known, "the generated action entities are missing")

	// Cross-check the generated entities against the generated table, so a
	// stale actions.json cannot make a typo look valid.
	for _, op := range operations {
		assert.Truef(t, known[op.Action],
			"%s is in the action table but not in %s; run 'go generate ./internal/authz/'",
			op.Action, actionsFile)
	}

	matches := actionRef.FindAllStringSubmatch(string(source), -1)
	require.NotEmpty(t, matches, "no policy names an action; the check would pass vacuously")

	for _, m := range matches {
		assert.Truef(t, known[m[1]],
			"%s names NKE::Action::%q, which no operation or action group declares", policiesFile, m[1])
	}
}

func TestEveryRoleAPolicyNamesExists(t *testing.T) {
	dir := bundleDir(t)

	source, err := os.ReadFile(filepath.Join(dir, policiesFile))
	require.NoError(t, err)

	known := entityIDs(t, filepath.Join(dir, rolesFile), "NKE::Role")
	require.NotEmpty(t, known, "no roles are declared")

	matches := roleRef.FindAllStringSubmatch(string(source), -1)
	require.NotEmpty(t, matches, "no policy names a role; the check would pass vacuously")

	for _, m := range matches {
		assert.Truef(t, known[m[1]],
			"%s names NKE::Role::%q, which %s does not declare", policiesFile, m[1], rolesFile)
	}
}

// A role nobody holds grants nothing, which is usually a mistake rather than an
// intention — and it is invisible, because the policy naming it still loads.
func TestEveryRoleIsHeldBySomeone(t *testing.T) {
	dir := bundleDir(t)

	raw, err := os.ReadFile(filepath.Join(dir, rolesFile))
	require.NoError(t, err)

	var entities []struct {
		UID struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"uid"`
		Parents []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"parents"`
	}
	require.NoError(t, json.Unmarshal(raw, &entities))

	declared := map[string]bool{}
	held := map[string]bool{}
	for _, e := range entities {
		if e.UID.Type == "NKE::Role" {
			declared[e.UID.ID] = true
		}
		// A parent link from anything — a principal, or a nested role — counts
		// as held: a role reachable from a subject grants something.
		for _, p := range e.Parents {
			if p.Type == "NKE::Role" {
				held[p.ID] = true
			}
		}
	}

	for role := range declared {
		assert.Truef(t, held[role],
			"NKE::Role::%q is declared but no principal or role is a member of it, so it grants nothing", role)
	}
}

// The other direction, which the two checks above leave open: a role that is
// declared, and held, and that no policy ever names.
//
// TestEveryRoleAPolicyNamesExists reads policies and asks whether roles.json
// declares them; TestEveryRoleIsHeldBySomeone reads roles.json and asks whether
// anyone is a member. A role appended with a holder and no matching permit
// satisfies both, reads in review as a grant, and grants nothing — which is the
// failure that looks like a working configuration for as long as nobody tries
// the thing it was supposed to allow.
func TestEveryDeclaredRoleIsNamedByAPolicy(t *testing.T) {
	dir := bundleDir(t)

	source, err := os.ReadFile(filepath.Join(dir, policiesFile))
	require.NoError(t, err)

	// Regex over the raw policy source, like roleRef everywhere else in this
	// file — which means a role name that only appears in a *comment* counts as
	// "named" too. That is a known, accepted limitation of a CI-only test: it
	// can be fooled by a comment that happens to spell a role, and it is not
	// worth a real Cedar parse to close a gap this narrow. Do not be surprised
	// that it is not caught here.
	named := map[string]bool{}
	for _, m := range roleRef.FindAllStringSubmatch(string(source), -1) {
		named[m[1]] = true
	}
	require.NotEmpty(t, named, "no policy names a role; the check would pass vacuously")

	declared := entityIDs(t, filepath.Join(dir, rolesFile), "NKE::Role")
	require.NotEmpty(t, declared, "no roles are declared")

	// A role nested under a role a policy names is named through that one: a
	// principal assigned the nested role is, through the entity hierarchy, also
	// "in" every ancestor of it, so a policy naming an ancestor matches them
	// too.
	parents := roleParents(t, filepath.Join(dir, rolesFile))
	for role := range declared {
		assert.Truef(t, roleIsNamed(role, named, parents),
			"NKE::Role::%q is declared and held, and no policy names it or any role that nests it, so it grants nothing",
			role)
	}
}

// roleIsNamed reports whether role is used: named directly by a policy, or
// nested (through one or more levels of roleParents) under a role that is.
//
// Reachability flows *up* the parent chain, starting from role itself: role is
// a member of everything in parents[role], and transitively of everything
// those roles are members of, so role is used if any ancestor in that chain is
// named — not if some other role nested *under* role happens to be named,
// which says nothing about whether role itself grants anything.
func roleIsNamed(role string, named map[string]bool, parents map[string]map[string]bool) bool {
	return roleIsNamedFrom(role, named, parents, map[string]bool{})
}

func roleIsNamedFrom(role string, named map[string]bool, parents map[string]map[string]bool, seen map[string]bool) bool {
	if named[role] {
		return true
	}
	if seen[role] {
		// A cycle in the role hierarchy is a different bug; do not spin on it.
		return false
	}
	seen[role] = true
	for parent := range parents[role] {
		if roleIsNamedFrom(parent, named, parents, seen) {
			return true
		}
	}
	return false
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

// roleParents maps each NKE::Role to the roles it is a member of.
func roleParents(t *testing.T, path string) map[string]map[string]bool {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var entities []struct {
		UID struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"uid"`
		Parents []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"parents"`
	}
	require.NoError(t, json.Unmarshal(raw, &entities))

	out := map[string]map[string]bool{}
	for _, e := range entities {
		if e.UID.Type != "NKE::Role" {
			continue
		}
		out[e.UID.ID] = map[string]bool{}
		for _, p := range e.Parents {
			if p.Type == "NKE::Role" {
				out[e.UID.ID][p.ID] = true
			}
		}
	}
	return out
}
