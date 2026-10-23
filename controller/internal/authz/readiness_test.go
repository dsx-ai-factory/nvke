// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What the readiness gate has to be able to tell apart.
//
// Both states below load without error, hold entities, and produce a pod that
// reports itself ready and then refuses traffic. Neither is a Cedar problem: the
// bundle is well-formed and the policies are fine. They are the two ways a
// bundle can be well-formed and useless, and a gate that counts entities cannot
// see either, because the generated action entities alone satisfy the count.

// writeBundle writes a bundle from its three Cedar parts, with aMapping as
// roleMappings.json, and returns the directory. roles must declare reader, and
// a bundle that is meant to load needs testIssuers passed to NewLocal. aMapping
// names testIssuer, so a test loading this bundle with nil issuers fails in
// checkIssuers, whatever else it meant to test.
func writeBundle(t *testing.T, policies, actions, roles string) string {
	t.Helper()
	return writeBundleWithMappings(t, policies, actions, roles, aMapping)
}

// writeBundleWithMappings writes a bundle from all four parts and returns the
// directory.
func writeBundleWithMappings(tb testing.TB, policies, actions, roles, mappings string) string {
	tb.Helper()

	dir := tb.TempDir()
	for name, content := range map[string]string{
		policiesFile:     policies,
		actionsFile:      actions,
		rolesFile:        roles,
		roleMappingsFile: mappings,
	} {
		require.NoError(tb, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	return dir
}

const aPolicy = `@id("readers-may-read")
permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource);`

const oneReader = `[{"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[]}]`

// aMapping grants reader to subjectReader from testIssuer.
const aMapping = `[{"issuer":"` + testIssuer + `","claim":{"name":"sub","value":"` + subjectReader + `"},
  "grants":[{"type":"NKE::Role","id":"reader"}]}]`

func shippedActions(t *testing.T) string {
	t.Helper()
	return shippedFile(t, actionsFile)
}

// shippedFile returns the named file from deploy/authz.
func shippedFile(tb testing.TB, name string) string {
	tb.Helper()
	raw, err := os.ReadFile(filepath.Join(bundleDir(tb), name))
	require.NoError(tb, err)
	return string(raw)
}

// roles.json emptied: 28 action entities still load, and nobody holds anything.
func TestABundleWithNoRolesIsRefused(t *testing.T) {
	_, err := NewLocal(writeBundleWithMappings(t, aPolicy, shippedActions(t), `[]`, `[]`), nil)
	require.Error(t, err, "a bundle nobody holds a role in refuses every request; it must not start")
	require.Contains(t, err.Error(), rolesFile+" declares no "+RoleType)
}

// Roles declared and no mapping granting them: the same outcome by a different
// edit.
func TestABundleWithNoMappingsIsRefused(t *testing.T) {
	_, err := NewLocal(writeBundleWithMappings(t, aPolicy, shippedActions(t), oneReader, `[]`), testIssuers)
	require.Error(t, err)
	require.Contains(t, err.Error(), roleMappingsFile+" has no entries: no caller would hold a role")
	require.Contains(t, err.Error(), "apiAuthz.roleMappingsConfigMap", "the error must name where grants come from")
}

// A principal entity grants its roles to a bare subject from every issuer, so
// the load refuses one in any entity file. The mapping in the same bundle is
// what the grant should be.
func TestABundleWithAPrincipalEntityIsRefused(t *testing.T) {
	const principal = `{"uid":{"type":"NKE::Principal","id":"` + subjectReader + `"},
  "attrs":{},"parents":[{"type":"NKE::Role","id":"reader"}]}`

	var actions []json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(shippedActions(t)), &actions))
	actionsWithPrincipal, err := json.Marshal(append(actions, json.RawMessage(principal)))
	require.NoError(t, err)

	for _, tt := range []struct {
		file           string
		actions, roles string
	}{
		{rolesFile, shippedActions(t), `[
 {"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[]},
 ` + principal + `
]`},
		{actionsFile, string(actionsWithPrincipal), oneReader},
	} {
		t.Run("in "+tt.file, func(t *testing.T) {
			_, err := NewLocal(writeBundle(t, aPolicy, tt.actions, tt.roles), testIssuers)
			require.Error(t, err, "a principal entity must not load, whichever file declares it")
			assert.Contains(t, err.Error(), tt.file+` declares NKE::Principal::"`+subjectReader+`", which would hold`,
				"the error must name the file that declared the principal")
			assert.Contains(t, err.Error(), "grant its roles with a "+roleMappingsFile+" entry naming the caller's issuer",
				"the error must say where a grant belongs")
		})
	}
}

// Past five principals the error summarizes the rest.
func TestThePrincipalRefusalListsAtMostFive(t *testing.T) {
	var roles strings.Builder
	roles.WriteString(`[{"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[]}`)
	for i := range 7 {
		fmt.Fprintf(&roles, `,{"uid":{"type":"NKE::Principal","id":"p%d"},"attrs":{},"parents":[]}`, i)
	}
	roles.WriteString(`]`)

	_, err := NewLocal(writeBundle(t, aPolicy, shippedActions(t), roles.String()), testIssuers)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `NKE::Principal::"p4" and 2 more, which would hold`)
	assert.NotContains(t, err.Error(), `"p5"`)
}

// The bundle is a mounted ConfigMap and the operations table is compiled in, so
// they are built at different times and can be different ages. An image that
// adds an endpoint against an older bundle refuses that endpoint to everyone —
// and, before this, reported itself ready while doing it.
func TestABundleOlderThanTheBinaryIsRefused(t *testing.T) {
	var entities []map[string]any
	require.NoError(t, json.Unmarshal([]byte(shippedActions(t)), &entities))

	const missing = "deleteCluster"
	kept := entities[:0]
	for _, e := range entities {
		uid, _ := e["uid"].(map[string]any)
		if uid["id"] == missing {
			continue
		}
		kept = append(kept, e)
	}
	require.Len(t, kept, len(entities)-1, "the action under test was not in the shipped entities")

	trimmed, err := json.Marshal(kept)
	require.NoError(t, err)

	_, err = NewLocal(writeBundle(t, aPolicy, string(trimmed), oneReader), testIssuers)
	require.Error(t, err, "an action this build authorizes with must exist in the bundle it was given")
	require.Contains(t, err.Error(), missing)
}

// A role's parent naming a role nothing declares: the role and mapping counts
// above stay green, and cedar-go's ancestry walk would still treat every
// holder of the role as a member of the undeclared one at request time
// (local.go's checkEntityStore doc comment, and overlayPrincipal's -- this is
// the roles.json half of the gap the mapped-role filter closes for
// roleMappings.json).
func TestABundleWhereARoleParentIsUndeclaredIsRefused(t *testing.T) {
	_, err := NewLocal(writeBundle(t, aPolicy, shippedActions(t), `[
 {"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[{"type":"NKE::Role","id":"operator"}]}
]`), testIssuers)
	require.Error(t, err, "a parent naming an undeclared role must refuse the bundle at load, "+
		"not silently grant membership in a role that was never loaded")
	require.Contains(t, err.Error(), `NKE::Role::"operator"`)
	require.Contains(t, err.Error(), `NKE::Role::"reader"`)
}

// Ready applies the same rules as the loader, from the same function, so the
// gate cannot come to mean something the loader does not.
func TestReadyHoldsTheShippedBundleToTheSameRules(t *testing.T) {
	local, err := NewLocal(shippedBundleWithMappings(t, testRoleMappings), testIssuers)
	require.NoError(t, err)
	require.NoError(t, local.Ready(t.Context()))

	// A Local assembled around an entity store that would not have loaded must
	// not pass the gate either.
	require.Error(t, (&Local{policies: local.policies}).Ready(t.Context()))
}
