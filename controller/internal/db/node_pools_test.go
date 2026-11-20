// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestNodePoolProviderGettersAndSetters(t *testing.T) {
	subnetID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	vpcPrefixID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	instanceTypeID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	ibPartitionID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	nvlinkPartitionID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	sshKeyGroupID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	machineTemplate := json.RawMessage(`{"kind":"ForgeMachineTemplate"}`)
	nodePool := db.NodePool{SystemReservedMemory: "32Gi"}

	nodePool.SetNico(db.NicoNodePoolProvider{
		SubnetIDs:                []uuid.UUID{subnetID},
		VpcPrefixIDs:             []uuid.UUID{vpcPrefixID},
		InstanceTypeID:           instanceTypeID,
		InfinibandPartitionID:    &ibPartitionID,
		NvLinkLogicalPartitionID: &nvlinkPartitionID,
		SSHKeyGroupIDs:           []uuid.UUID{sshKeyGroupID},
		Release:                  "nke-260318",
		ConfigProfile:            "gpu",
		DiskSelectionStrategy:    db.DiskSelectionStrategyLargest,
	})

	require.Equal(t, []uuid.UUID{subnetID}, nodePool.Nico.SubnetIDs)
	require.Nil(t, nodePool.Capi)
	require.Equal(t, []uuid.UUID{vpcPrefixID}, nodePool.Nico.VpcPrefixIDs)
	require.Equal(t, instanceTypeID, nodePool.Nico.InstanceTypeID)
	require.Equal(t, ibPartitionID, *nodePool.Nico.InfinibandPartitionID)
	require.Equal(t, nvlinkPartitionID, *nodePool.Nico.NvLinkLogicalPartitionID)
	require.Equal(t, []uuid.UUID{sshKeyGroupID}, nodePool.Nico.SSHKeyGroupIDs)
	require.Equal(t, "nke-260318", nodePool.Nico.Release)
	require.Equal(t, "gpu", nodePool.Nico.ConfigProfile)
	require.Equal(t, db.DiskSelectionStrategyLargest, nodePool.Nico.DiskSelectionStrategy)

	nicoNodePoolProvider, err := nodePool.NicoProvider()
	require.NoError(t, err)
	require.Equal(t, instanceTypeID, nicoNodePoolProvider.InstanceTypeID)
	_, err = nodePool.CAPIProvider()
	require.Error(t, err)
	require.ErrorIs(t, err, db.ErrMissingProviderPayload)

	// Setting one provider clears the other, so a pool never carries both.
	nodePool.SetCapi(db.CAPINodePoolProvider{
		InfraMachineTemplate:   machineTemplate,
		BootstrapFiles:         []db.CloudInitFile{{Path: "/etc/test", Content: "ok"}},
		PreKubeadmCommands:     []string{"echo pre"},
		SpireAttestationMethod: db.SpireAttestationMethodJoinToken,
	})
	require.Nil(t, nodePool.Nico)
	require.JSONEq(t, string(machineTemplate), string(nodePool.Capi.InfraMachineTemplate))
	require.Equal(t, []db.CloudInitFile{{Path: "/etc/test", Content: "ok"}}, nodePool.Capi.BootstrapFiles)
	require.Equal(t, []string{"echo pre"}, nodePool.Capi.PreKubeadmCommands)
	require.Equal(t, db.SpireAttestationMethodJoinToken, nodePool.Capi.SpireAttestationMethod)

	capiNodePoolProvider, err := nodePool.CAPIProvider()
	require.NoError(t, err)
	require.JSONEq(t, string(machineTemplate), string(capiNodePoolProvider.InfraMachineTemplate))
	_, err = nodePool.NicoProvider()
	require.Error(t, err)
	require.ErrorIs(t, err, db.ErrMissingProviderPayload)

	nextSubnetID := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	nextInstanceTypeID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	nodePool.SetNico(db.NicoNodePoolProvider{
		SubnetIDs:             []uuid.UUID{nextSubnetID},
		InstanceTypeID:        nextInstanceTypeID,
		Release:               "next-os",
		ConfigProfile:         "next-config",
		DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
	})
	require.Equal(t, nextInstanceTypeID, nodePool.Nico.InstanceTypeID)
	require.Equal(t, "next-os", nodePool.Nico.Release)
	require.Equal(t, "next-config", nodePool.Nico.ConfigProfile)
	require.Equal(t, "32Gi", nodePool.SystemReservedMemory)
}

func TestNodePoolResolveProviderType(t *testing.T) {
	nico := db.NodePool{ID: uuid.New()}
	nico.SetNico(db.NicoNodePoolProvider{})
	providerType, err := nico.ResolveProviderType()
	require.NoError(t, err)
	require.Equal(t, db.ProviderTypeNico, providerType)

	capi := db.NodePool{ID: uuid.New()}
	capi.SetCapi(db.CAPINodePoolProvider{})
	providerType, err = capi.ResolveProviderType()
	require.NoError(t, err)
	require.Equal(t, db.ProviderTypeCAPI, providerType)

	empty := db.NodePool{ID: uuid.New()}
	_, err = empty.ResolveProviderType()
	require.ErrorIs(t, err, db.ErrMissingProviderPayload)
}

func TestNodePool_MachineMetadataPersistence(t *testing.T) {
	type parameters struct{ annotations, labels map[string]string }
	for name, params := range map[string]parameters{
		"omitted":    {},
		"configured": {annotations: map[string]string{"example.com/setup": "gpu,network"}, labels: map[string]string{"example.com/pool": "gpu"}},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			bunDB := testutil.SetupDB(ctx, t, (*db.NodePool)(nil))
			queries := db.NewDB(bunDB)
			pool := db.NodePool{
				ID: uuid.New(), ClusterID: uuid.New(), Name: "workers", NodeNamePrefix: "worker", KubernetesVersion: "1.32.1",
				MachineAnnotations: params.annotations, MachineLabels: params.labels,
			}
			_, err := queries.CreateNodePool(ctx, &pool)
			require.NoError(t, err)
			loaded, err := queries.GetNodePool(ctx, pool.ID)
			require.NoError(t, err)
			require.Equal(t, db.CloneStringMap(params.annotations), loaded.MachineAnnotations)
			require.Equal(t, db.CloneStringMap(params.labels), loaded.MachineLabels)
			// Other pool updates retain the immutable metadata.
			loaded.DesiredCount = 2
			_, err = queries.UpdateNodePool(ctx, &loaded)
			require.NoError(t, err)
			loaded, err = queries.GetNodePool(ctx, pool.ID)
			require.NoError(t, err)
			require.Equal(t, db.CloneStringMap(params.annotations), loaded.MachineAnnotations)
			require.Equal(t, db.CloneStringMap(params.labels), loaded.MachineLabels)
		})
	}
}

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
