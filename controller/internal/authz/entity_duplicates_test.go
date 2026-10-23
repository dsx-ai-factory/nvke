// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A duplicate entity is a silent grant.
//
// The entity files are lists, and the thing they load into is a map, so two
// declarations of one uid are not an error anywhere in encoding/json — the
// second simply replaces the first. What that means here is that a hand edit
// repeating a principal with a wider parent set makes the reader an operator,
// with the bundle loading clean, the schema validating and the conformance
// matrix passing, because by the time any of them look there is one entity.

// entityBundle writes a bundle whose roles file is given verbatim, keeping the
// real generated actions so the failure under test is the entity merge and not
// a missing action group.
func entityBundle(t *testing.T, roles string) string {
	t.Helper()

	dir := t.TempDir()
	actions, err := os.ReadFile(filepath.Join(bundleDir(t), actionsFile))
	require.NoError(t, err)

	for name, content := range map[string]string{
		policiesFile: `@id("readers-may-read")
permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource);`,
		actionsFile: string(actions),
		rolesFile:   roles,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	return dir
}

func TestADuplicateWithinOneFileIsRefused(t *testing.T) {
	dir := entityBundle(t, `[
 {"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[]},
 {"uid":{"type":"NKE::Role","id":"operator"},"attrs":{},"parents":[{"type":"NKE::Role","id":"reader"}]},
 {"uid":{"type":"NKE::Principal","id":"system:serviceaccount:nke-system:nke-cli"},
  "attrs":{},"parents":[{"type":"NKE::Role","id":"reader"}]},
 {"uid":{"type":"NKE::Principal","id":"system:serviceaccount:nke-system:nke-cli"},
  "attrs":{},"parents":[{"type":"NKE::Role","id":"operator"}]}
]`)

	_, err := NewLocal(dir)
	require.Error(t, err, "a principal declared twice must not load; the second declaration decides what it holds")
	require.Contains(t, err.Error(), "redeclares")
	require.Contains(t, err.Error(), rolesFile, "the error must name the file to edit")
}

func TestADuplicateAcrossFilesIsRefused(t *testing.T) {
	// "read" is one of the generated action groups, so declaring it again in
	// roles.json is the cross-file case: two files, one uid.
	dir := entityBundle(t, `[
 {"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[]},
 {"uid":{"type":"NKE::Action","id":"read"},"attrs":{},"parents":[]},
 {"uid":{"type":"NKE::Principal","id":"system:serviceaccount:nke-system:nke-cli"},
  "attrs":{},"parents":[{"type":"NKE::Role","id":"reader"}]}
]`)

	_, err := NewLocal(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), actionsFile, "the error must name where the uid was first declared")
}

// The bundle that ships must not trip either check, or the tests above are
// asserting something the deployment does not hold to.
func TestTheShippedBundleDeclaresEachEntityOnce(t *testing.T) {
	_, err := NewLocal(bundleDir(t))
	require.NoError(t, err)
}
