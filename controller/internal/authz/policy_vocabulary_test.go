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

// A policy naming an action that does not exist parses cleanly and then never
// matches — a rule that looks enforced and is not. cedar-agent catches this when
// a schema is loaded, and says "did you mean"; cedar-go's schema validator is
// still under x/exp, so it is not used here.
//
// This closes the same gap without the experimental dependency, and closes it
// for both engines: every action and role a policy names must exist in the
// generated action table or in the entity files beside it. It is a narrower
// check than a schema — it does not type attributes — but it covers the class of
// mistake that is actually silent.

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
