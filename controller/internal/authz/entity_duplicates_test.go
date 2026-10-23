// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A duplicate entity is a silent grant.
//
// The entity files are lists, and the thing they load into is a map, so two
// declarations of one uid are not an error anywhere in encoding/json — the
// second simply replaces the first. What that means here is that a hand edit
// repeating a role with a wider parent set makes every reader an operator,
// with the bundle loading clean, the schema validating and the conformance
// matrix passing, because by the time any of them look there is one entity.

func TestADuplicateWithinOneFileIsRefused(t *testing.T) {
	dir := writeBundle(t, aPolicy, shippedActions(t), `[
 {"uid":{"type":"NKE::Role","id":"operator"},"attrs":{},"parents":[]},
 {"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[]},
 {"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[{"type":"NKE::Role","id":"operator"}]}
]`)

	_, err := NewLocal(dir, testIssuers)
	require.Error(t, err, "a role declared twice must not load; the second declaration decides what it nests in")
	require.Contains(t, err.Error(), "redeclares")
	require.Contains(t, err.Error(), rolesFile, "the error must name the file to edit")
}

func TestADuplicateAcrossFilesIsRefused(t *testing.T) {
	// "read" is one of the generated action groups, so declaring it again in
	// roles.json is the cross-file case: two files, one uid.
	dir := writeBundle(t, aPolicy, shippedActions(t), `[
 {"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[]},
 {"uid":{"type":"NKE::Action","id":"read"},"attrs":{},"parents":[]}
]`)

	_, err := NewLocal(dir, testIssuers)
	require.Error(t, err)
	require.Contains(t, err.Error(), actionsFile, "the error must name where the uid was first declared")
}

// The bundle that ships must not trip either check, or the tests above are
// asserting something the deployment does not hold to.
func TestTheShippedBundleDeclaresEachEntityOnce(t *testing.T) {
	_, err := NewLocal(shippedBundleWithMappings(t, testRoleMappings), testIssuers)
	require.NoError(t, err)
}
