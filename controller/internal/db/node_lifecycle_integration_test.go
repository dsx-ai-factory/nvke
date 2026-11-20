// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0.

//go:build integration

package db_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// TestListNodeLifecycleElapsedIncludesControlPlaneNodes covers the fix for the
// metric's query: control-plane nodes managed by the control-plane controller
// have no node set or node pool, so the query joins clusters on the node's
// cluster_id, LEFT JOINs node_sets and node_pools, and takes node_type from
// n.control_plane — every such node in a lifecycle state gets a series with
// node_type="cp". It runs on a real migrated Postgres because the query uses
// EXTRACT(EPOCH FROM ...) and jsonb operators that SQLite cannot parse.
func TestListNodeLifecycleElapsedIncludesControlPlaneNodes(t *testing.T) {
	bunDB := newMigratedPostgres(t.Context(), t)
	store := db.NewDB(bunDB)

	cluster := newValidCluster(withName("lifecycle-cp-" + uuid.NewString()[:8]))
	_, err := store.CreateCluster(t.Context(), &cluster)
	require.NoError(t, err)

	at := time.Now().UTC().Add(-time.Minute)
	newCond := func(on bool, when time.Time) db.Condition {
		return db.Condition{Status: db.ConditionStatusFromBool(on), LastTransitionTime: when}
	}

	names := map[string]string{"pending": "", "provisioning": "", "deleting": "", "markedForRecycling": ""}
	for state := range names {
		node := db.Node{
			ID:                     uuid.New(),
			ClusterID:              cluster.ID,
			ControlPlane:           true, // no node pool and no node set: control-plane controller managed
			Name:                   "cp-" + state,
			IP:                     "10.0.0.20",
			SpireAttestationMethod: db.SpireAttestationMethodTPM,
		}
		node.Conditions = db.NodeConditions{
			Pending:            newCond(state == "pending", at),
			Provisioning:       newCond(state == "provisioning", at),
			Deleting:           newCond(state == "deleting", at),
			MarkedForRecycling: newCond(state == "markedForRecycling", at),
		}
		created, err := store.CreateNode(t.Context(), &node)
		require.NoError(t, err)
		names[state] = created.Name
	}

	rows, err := store.ListNodeLifecycleElapsed(t.Context())
	require.NoError(t, err)

	seen := map[string]string{}
	for _, r := range rows {
		require.Equal(t, "cp", r.NodeType, "node %s: node_type comes from n.control_plane", r.NodeName)
		require.Equal(t, cluster.Name, r.ClusterName, "the cluster comes from n.cluster_id")
		require.GreaterOrEqual(t, r.ElapsedSecs, float64(0))
		seen[r.NodeName] = r.State
	}
	for state, name := range names {
		require.Contains(t, seen, name, "a control-plane node in state %s has a series", state)
		require.Equal(t, state, seen[name])
	}
}
