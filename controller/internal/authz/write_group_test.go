// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// allWrites is every write operation's action in the compiled table, sorted.
func allWrites(t *testing.T) []string {
	t.Helper()
	var writes []string
	for pattern, op := range operations {
		if !safeMethod(pattern) {
			writes = append(writes, op.Action)
		}
	}
	slices.Sort(writes)
	require.NotEmpty(t, writes, "the operations table has no writes; every case below would pass vacuously")
	return writes
}

func without(list []string, drop string) []string {
	return slices.DeleteFunc(slices.Clone(list), func(s string) bool { return s == drop })
}

// Each case is loaded through loadBundle, so what is asserted is what a
// deployment reports, with the policy parsed the way the engine parses it.
func TestGroupOnlyWrites(t *testing.T) {
	writes := allWrites(t)
	require.Contains(t, writes, "deleteCluster")

	const (
		readGrant  = `permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource);`
		writeGrant = `permit(principal in NKE::Role::"reader", action in NKE::Action::"write", resource);`
		named      = `forbid(principal, action == NKE::Action::"deleteCluster", resource);`
	)
	for _, tt := range []struct {
		name   string
		policy string
		want   []string
	}{
		{"a policy that never names write leaves nothing to it", readGrant, nil},
		{"granting write leaves every write to it", writeGrant, writes},
		{"a write named in a forbid is not left to the group", writeGrant + "\n" + named, without(writes, "deleteCluster")},
		{
			"a write named in a permit is not left to the group",
			writeGrant + "\n" + `permit(principal in NKE::Role::"reader", action in [NKE::Action::"read", NKE::Action::"deleteCluster"], resource);`,
			without(writes, "deleteCluster"),
		},
		{"forbidding write alone grants nothing through it", readGrant + "\n" + `forbid(principal, action in NKE::Action::"write", resource);`, nil},
		{"an unconstrained action scope grants the group", `permit(principal in NKE::Role::"reader", action, resource);`, writes},
		{
			"write in an action list grants the group",
			`permit(principal in NKE::Role::"reader", action in [NKE::Action::"read", NKE::Action::"write"], resource);`, writes,
		},
		{
			"spacing inside the reference does not hide it",
			`permit(principal in NKE::Role::"reader", action in NKE :: Action :: "write", resource);` + "\n" +
				`forbid(principal, action == NKE :: Action :: "deleteCluster", resource);`,
			without(writes, "deleteCluster"),
		},
		{"write named only in a comment is not named", readGrant + "\n// " + writeGrant, nil},
		{"an action named only in a comment is still left to the group", writeGrant + "\n// " + named, writes},
		{
			"an action named only in a condition is still left to the group",
			writeGrant + "\n" + `forbid(principal, action, resource) when { action == NKE::Action::"deleteCluster" && false };`, writes,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, errs := loadBundle(writeBundle(t, tt.policy, shippedActions(t), oneReader))
			require.NotNil(t, b, "%v", errs)
			assert.Equal(t, tt.want, b.groupOnlyWrites)
		})
	}
}

// On the shipped bundle the report is exactly the fence's acknowledged
// exceptions: the fence refuses any other write left to the group, and every
// exception must be left to it. So the gauge's shipped value is
// len(writesLeftToTheGroup), and a change in it on a custom bundle is what an
// alert keys on.
func TestTheShippedBundleLeavesOnlyItsAcknowledgedWritesToTheGroup(t *testing.T) {
	b, errs := loadBundle(shippedBundleWithMappings(t, testRoleMappings))
	require.Empty(t, errs)

	want := slices.Sorted(maps.Keys(writesLeftToTheGroup))
	assert.Equal(t, want, b.groupOnlyWrites)
}

// Setup reports what it loaded: one log line per group-only write, and the
// count to the recorder.
func TestSetupReportsGroupOnlyWrites(t *testing.T) {
	var log bytes.Buffer
	var recorded []int
	provider, err := Setup(t.Context(), Options{
		BundleDir:              shippedBundleWithMappings(t, testRoleMappings),
		IssuerNames:            testIssuers,
		GroupOnlyWriteRecorder: func(n int) { recorded = append(recorded, n) },
	}, slog.New(slog.NewJSONHandler(&log, nil)))
	require.NoError(t, err)
	require.NotNil(t, provider)

	assert.Equal(t, []int{len(writesLeftToTheGroup)}, recorded)

	var logged []string
	for line := range bytes.Lines(log.Bytes()) {
		var entry struct {
			Msg    string `json:"msg"`
			Action string `json:"action"`
		}
		require.NoError(t, json.Unmarshal(line, &entry))
		if entry.Msg == "write operation the policy covers only through the write action group" {
			logged = append(logged, entry.Action)
		}
	}
	assert.ElementsMatch(t, slices.Collect(maps.Keys(writesLeftToTheGroup)), logged)
}
