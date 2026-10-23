// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// What the readiness gate has to be able to tell apart.
//
// Both states below load without error, hold entities, and produce a pod that
// reports itself ready and then refuses traffic. Neither is a Cedar problem: the
// bundle is well-formed and the policies are fine. They are the two ways a
// bundle can be well-formed and useless, and a gate that counts entities cannot
// see either, because the generated action entities alone satisfy the count.

// writeBundle writes a bundle from its three parts and returns the directory.
func writeBundle(t *testing.T, policies, actions, roles string) string {
	t.Helper()

	dir := t.TempDir()
	for name, content := range map[string]string{
		policiesFile: policies,
		actionsFile:  actions,
		rolesFile:    roles,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	return dir
}

const aPolicy = `@id("readers-may-read")
permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource);`

func shippedActions(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), actionsFile))
	require.NoError(t, err)
	return string(raw)
}

// roles.json emptied: 28 action entities still load, and nobody holds anything.
func TestABundleWithNoRolesIsRefused(t *testing.T) {
	_, err := NewLocal(writeBundle(t, aPolicy, shippedActions(t), `[]`))
	require.Error(t, err, "a bundle nobody holds a role in refuses every request; it must not start")
	require.Contains(t, err.Error(), rolesFile)
}

// Roles declared, nobody holding them: the same outcome by a different edit.
func TestABundleWithNoPrincipalsIsRefused(t *testing.T) {
	_, err := NewLocal(writeBundle(t, aPolicy, shippedActions(t),
		`[{"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[]}]`))
	require.Error(t, err)
	require.Contains(t, err.Error(), PrincipalType)
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

	_, err = NewLocal(writeBundle(t, aPolicy, string(trimmed), `[
 {"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[]},
 {"uid":{"type":"NKE::Principal","id":"system:serviceaccount:nke-system:nke-cli"},
  "attrs":{},"parents":[{"type":"NKE::Role","id":"reader"}]}
]`))
	require.Error(t, err, "an action this build authorizes with must exist in the bundle it was given")
	require.Contains(t, err.Error(), missing)
}

// A principal's static parent naming a role nothing declares: roles has at
// least one entry, principals has at least one entry, so the two counts above
// stay green, and cedar-go's ancestry walk would still treat this principal as
// a member of the undeclared role at request time (local.go's checkEntityStore
// doc comment, and overlayPrincipal's -- this is the static half of the same
// gap the claim-derived filter closes for Query.ClaimRoles).
func TestABundleWhereAStaticParentIsUndeclaredIsRefused(t *testing.T) {
	_, err := NewLocal(writeBundle(t, aPolicy, shippedActions(t), `[
 {"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[]},
 {"uid":{"type":"NKE::Principal","id":"system:serviceaccount:nke-system:nke-cli"},
  "attrs":{},"parents":[{"type":"NKE::Role","id":"operator"}]}
]`))
	require.Error(t, err, "a static parent naming an undeclared role must refuse the bundle at load, "+
		"not silently grant membership in a role that was never loaded")
	require.Contains(t, err.Error(), `NKE::Role::"operator"`)
	require.Contains(t, err.Error(), `system:serviceaccount:nke-system:nke-cli`)
}

// Ready applies the same rules as the loader, from the same function, so the
// gate cannot come to mean something the loader does not.
func TestReadyHoldsTheShippedBundleToTheSameRules(t *testing.T) {
	local, err := NewLocal(bundleDir(t))
	require.NoError(t, err)
	require.NoError(t, local.Ready(t.Context()))

	// A Local assembled around an entity store that would not have loaded must
	// not pass the gate either.
	require.Error(t, (&Local{policies: local.policies}).Ready(t.Context()))
}
