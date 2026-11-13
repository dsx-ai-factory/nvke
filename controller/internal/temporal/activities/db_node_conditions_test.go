// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

// TestUpdateNodeConditionsSkipsUnchangedWrite reproduces #2263: the same update
// applied twice writes the node row once. A write is what enqueues the node on
// every node source, so no write also means no extra reconcile.
func TestUpdateNodeConditionsSkipsUnchangedWrite(t *testing.T) {
	store := newFenceStore()
	store.cluster.Conditions.UpdateInProgress = nil
	a := fenceActivities(store, nil)
	update := activities.NodeConditionsUpdate{
		NodeID:     store.nodes[0].ID,
		Conditions: activities.NodeConditions{Pending: &activities.ConditionUpdate{Status: db.ConditionStatusTrue, Message: "waiting for machine"}},
	}

	require.NoError(t, a.UpdateNodeConditions(t.Context(), update))
	require.Equal(t, 1, store.writes)
	written := store.nodes[0].Conditions

	require.NoError(t, a.UpdateNodeConditions(t.Context(), update))
	require.Equal(t, 1, store.writes, "re-asserting the stored conditions wrote the row again")
	require.True(t, store.nodes[0].Conditions.Equal(written))
}

func TestUpdateNodeConditionsWritesOnlyOnChange(t *testing.T) {
	for _, tc := range []struct {
		name       string
		conditions activities.NodeConditions
		writes     int
	}{
		{
			name:       "same status and message",
			conditions: activities.NodeConditions{NodeAgentReady: &activities.ConditionUpdate{Status: db.ConditionStatusTrue, Message: "ready"}},
			writes:     0,
		},
		{
			name:       "no conditions requested",
			conditions: activities.NodeConditions{},
			writes:     0,
		},
		{
			name:       "message change",
			conditions: activities.NodeConditions{NodeAgentReady: &activities.ConditionUpdate{Status: db.ConditionStatusTrue, Message: "ready again"}},
			writes:     1,
		},
		{
			name:       "status change",
			conditions: activities.NodeConditions{NodeAgentReady: &activities.ConditionUpdate{Status: db.ConditionStatusFalse, Message: "ready"}},
			writes:     1,
		},
		{
			name:       "condition set for the first time",
			conditions: activities.NodeConditions{Rebooting: &activities.ConditionUpdate{Status: db.ConditionStatusFalse, Message: "not rebooting"}},
			writes:     1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFenceStore()
			store.cluster.Conditions.UpdateInProgress = nil
			a := fenceActivities(store, nil)
			before := store.nodes[0].Conditions

			require.NoError(t, a.UpdateNodeConditions(t.Context(), activities.NodeConditionsUpdate{NodeID: store.nodes[0].ID, Conditions: tc.conditions}))
			require.Equal(t, tc.writes, store.writes)
			require.Equal(t, tc.writes == 0, store.nodes[0].Conditions.Equal(before))
		})
	}
}
