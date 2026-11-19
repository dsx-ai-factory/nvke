// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestListNodePoolCounts(t *testing.T) {
	ctx := t.Context()
	database := testutil.SetupDB(ctx, t, (*db.NodeSet)(nil), (*db.Node)(nil))
	exerciseNodePoolCounts(t, database)
}

func exerciseNodePoolCounts(t *testing.T, database *bun.DB) {
	t.Helper()
	ctx := t.Context()
	queries := db.NewDB(database)
	currentID, previousID, otherID := uuid.New(), uuid.New(), uuid.New()
	pool := db.NodePool{ID: uuid.New(), CurrentNodeSetID: &currentID, KubernetesVersion: "1.34.1", SystemReservedMemory: "16Gi"}
	pool.SetCapi(db.CAPINodePoolProvider{
		InfraMachineTemplate: json.RawMessage(`{"kind":"DockerMachineTemplate","spec":{"image":"new"}}`),
		BootstrapFiles:       []db.CloudInitFile{{Path: "/etc/example", Content: "new"}},
		PreKubeadmCommands:   []string{"echo new"},
	})
	emptyPool := pool
	emptyPool.ID = uuid.New()
	snapshot := db.NodeSet{ID: currentID, NodePoolID: pool.ID, KubernetesVersion: pool.KubernetesVersion, SystemReservedMemory: pool.SystemReservedMemory}
	snapshot.SetCapi(*pool.Capi)
	previous := snapshot
	previous.ID = previousID
	previous.KubernetesVersion = "1.33.1"
	other := snapshot
	other.ID, other.NodePoolID = otherID, uuid.New()
	snapshots := []db.NodeSet{snapshot, previous, other}
	_, err := database.NewInsert().Model(&snapshots).Exec(ctx)
	require.NoError(t, err)

	addNode := func(poolID, setID uuid.UUID, conditions db.NodeConditions) {
		t.Helper()
		node := db.Node{ID: uuid.New(), ClusterID: uuid.New(), NodePoolID: poolID, NodeSetID: setID, Conditions: conditions}
		_, err := database.NewInsert().Model(&node).Exec(ctx)
		require.NoError(t, err)
	}
	ready := db.NodeConditions{KubernetesReady: db.NewCondition(true, "Ready")}
	addNode(pool.ID, currentID, ready)
	addNode(pool.ID, currentID, db.NodeConditions{Pending: db.NewCondition(true, "Pending")})
	addNode(pool.ID, currentID, db.NodeConditions{Provisioning: db.NewCondition(true, "Provisioning")})
	deleting := ready
	deleting.Deleting = db.NewCondition(true, "Deleting")
	addNode(pool.ID, currentID, deleting)
	addNode(pool.ID, previousID, ready)
	addNode(pool.ID, previousID, db.NodeConditions{KubernetesReady: db.Condition{Status: db.ConditionStatusUnknown}})
	addNode(other.NodePoolID, otherID, ready)
	addNode(uuid.Nil, uuid.Nil, ready) // No-nodepool control plane is not included.

	counts, err := queries.ListNodePoolCounts(ctx, []db.NodePool{pool, emptyPool})
	require.NoError(t, err)
	require.Len(t, counts, 2)
	require.Equal(t, db.NodePoolCounts{NodeCount: 6, ReadyNodeCount: 3, UpdatedNodeCount: 4}, counts[pool.ID])
	require.Equal(t, db.NodePoolCounts{}, counts[emptyPool.ID])

	// Desired changes must be reflected even before currentNodeSetID advances.
	changed := pool
	changed.KubernetesVersion = "1.35.0"
	counts, err = queries.ListNodePoolCounts(ctx, []db.NodePool{changed})
	require.NoError(t, err)
	require.Equal(t, 0, counts[pool.ID].UpdatedNodeCount)

	// Rollback counts matching previous snapshots, independent of the active ID.
	changed.KubernetesVersion = previous.KubernetesVersion
	counts, err = queries.ListNodePoolCounts(ctx, []db.NodePool{changed})
	require.NoError(t, err)
	require.Equal(t, 2, counts[pool.ID].UpdatedNodeCount)

	counts, err = queries.ListNodePoolCounts(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, counts)

	require.NoError(t, database.Close())
	_, err = queries.ListNodePoolCounts(ctx, []db.NodePool{pool})
	require.ErrorContains(t, err, "aggregate node pool counts")
}

func TestNodePoolMatchesNodeSet(t *testing.T) {
	pool := db.NodePool{KubernetesVersion: "1.34.1", SystemReservedMemory: "16Gi", KubeReservedMemory: "2Gi"}
	pool.SetCapi(db.CAPINodePoolProvider{
		InfraMachineTemplate:   json.RawMessage(`{"kind":"DockerMachineTemplate","image":"new"}`),
		BootstrapFiles:         []db.CloudInitFile{{Path: "/etc/example", Content: "new"}},
		PreKubeadmCommands:     []string{"echo new"},
		SpireAttestationMethod: db.SpireAttestationMethodJoinToken,
	})
	snapshot := db.NodeSet{ID: uuid.New(), KubernetesVersion: pool.KubernetesVersion, SystemReservedMemory: pool.SystemReservedMemory, KubeReservedMemory: pool.KubeReservedMemory}
	snapshot.SetCapi(*pool.Capi)
	tests := []struct {
		name   string
		change func(*db.NodeSet)
	}{
		{"kubernetes version", func(s *db.NodeSet) { s.KubernetesVersion = "1.33.1" }},
		{"image template", func(s *db.NodeSet) { s.Capi.InfraMachineTemplate = json.RawMessage(`{"image":"old"}`) }},
		{"initialization files", func(s *db.NodeSet) { s.Capi.BootstrapFiles[0].Content = "old" }},
		{"initialization commands", func(s *db.NodeSet) { s.Capi.PreKubeadmCommands[0] = "echo old" }},
		{"attestation", func(s *db.NodeSet) { s.Capi.SpireAttestationMethod = db.SpireAttestationMethodTPM }},
		{"system memory", func(s *db.NodeSet) { s.SystemReservedMemory = "32Gi" }},
		{"kube memory", func(s *db.NodeSet) { s.KubeReservedMemory = "4Gi" }},
		{"teleport", func(s *db.NodeSet) { s.TeleportEnabled = true }},
		{"missing provider", func(s *db.NodeSet) { s.Capi = nil }},
	}
	// PostgreSQL JSONB can reorder keys and add whitespace, including in a
	// PATCH response whose desired template has not yet been re-read.
	reformatted := snapshot
	reformatted.SetCapi(*snapshot.Capi)
	reformatted.Capi.InfraMachineTemplate = json.RawMessage(`{ "image": "new", "kind": "DockerMachineTemplate" }`)
	require.True(t, db.NodePoolMatchesNodeSet(pool, reformatted))
	// A match doesn't require an active-generation pointer.
	require.True(t, db.NodePoolMatchesNodeSet(pool, snapshot))
	require.True(t, db.NodePoolNeedsNewNodeSet(pool, snapshot))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := snapshot
			changed.SetCapi(*snapshot.Capi)
			tt.change(&changed)
			require.False(t, db.NodePoolMatchesNodeSet(pool, changed))
		})
	}
	for _, tt := range []struct {
		name   string
		change func(*db.NicoNodePoolProvider)
	}{
		{"release", func(p *db.NicoNodePoolProvider) { p.Release = "old" }},
		{"config profile", func(p *db.NicoNodePoolProvider) { p.ConfigProfile = "old" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pool.SetNico(db.NicoNodePoolProvider{Release: "new", ConfigProfile: "new"})
			snapshot.SetNico(*pool.Nico)
			require.True(t, db.NodePoolMatchesNodeSet(pool, snapshot))
			tt.change(snapshot.Nico)
			require.False(t, db.NodePoolMatchesNodeSet(pool, snapshot))
		})
	}
	pool.SetLaunchLayer(db.LaunchLayerNodePoolProvider{HostOSProfile: "new", VMProfile: "new"})
	snapshot.SetLaunchLayer(*pool.LaunchLayer)
	require.True(t, db.NodePoolMatchesNodeSet(pool, snapshot))
	snapshot.LaunchLayer.HostOSProfile = "old"
	require.False(t, db.NodePoolMatchesNodeSet(pool, snapshot))
}
