// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodes_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/nodes"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

// nicoNode builds a node as a stored row looks: the instance type the scheduler
// chose lives in the nested nico payload, not at the row root.
func nicoNode(instanceTypeID uuid.UUID) db.Node {
	node := db.Node{}
	node.SetNico(db.NicoNodeProvider{InstanceTypeID: instanceTypeID})
	return node
}

func TestGetNodeContext_NoNodePoolControlPlane(t *testing.T) {
	ctx := context.Background()
	bunDB := testutil.SetupDB(ctx, t,
		(*db.Cluster)(nil),
		(*db.ControlPlaneConfiguration)(nil),
		(*db.Node)(nil),
	)
	database := db.NewDB(bunDB)

	siteID := uuid.New()
	cluster := db.Cluster{
		ID:                uuid.New(),
		Name:              "cp-cluster",
		KubernetesVersion: "1.31.0",
		TeleportEnabled:   true,
		ProviderType:      db.ProviderTypeNico,
		Nico: &db.NicoClusterProvider{
			SiteID: siteID,
			OrgID:  "test-org",
			VpcID:  uuid.New(),
		},
	}
	_, err := database.CreateCluster(ctx, &cluster)
	require.NoError(t, err)

	var snapshotCfg db.ControlPlaneConfigSpec
	snapshotCfg.KubeReservedMemory = "250Mi"
	snapshotCfg.TeleportEnabled = true
	snapshotCfg.ProvisioningMonitorDisabled = true
	// Empty DiskSelectionStrategy: ControlPlaneConfigNodeContext defaults it to smallest
	// when populating the synthetic NodeSet Nico payload.
	snapshotCfg.SetNico(db.NicoControlPlaneProvider{
		OSProfile:             "os",
		ConfigProfile:         "cfg",
		DiskSelectionStrategy: "",
	})
	snapshot, err := database.CreateControlPlaneConfiguration(ctx, &db.ControlPlaneConfiguration{
		ClusterID: cluster.ID,
		Config:    snapshotCfg,
	})
	require.NoError(t, err)
	cluster.ControlPlaneConfig.ConfigurationID = snapshot.ID
	_, err = database.UpdateCluster(ctx, &cluster)
	require.NoError(t, err)

	instanceTypeID := uuid.New()
	node := db.Node{
		ID:                     uuid.New(),
		SpireAttestationMethod: db.SpireAttestationMethodTPM,
		ClusterID:              cluster.ID,
		ControlPlane:           true,
		Name:                   "cp-1",
		IP:                     "10.0.0.1",
	}
	node.SetNico(db.NicoNodeProvider{InstanceTypeID: instanceTypeID})
	_, err = database.CreateNode(ctx, &node)
	require.NoError(t, err)

	nodeSet, nodePool, err := nodes.GetNodeContext(ctx, database, node)
	require.NoError(t, err)
	assert.Equal(t, uuid.Nil, nodeSet.ID)
	assert.Equal(t, uuid.Nil, nodePool.ID)
	nicoNodeSetProvider, err := nodeSet.NicoProvider()
	require.NoError(t, err)
	assert.Equal(t, db.DiskSelectionStrategySmallest, nicoNodeSetProvider.DiskSelectionStrategy)
	nicoNodePoolProvider, err := nodePool.NicoProvider()
	require.NoError(t, err)
	assert.Equal(t, db.DiskSelectionStrategySmallest, nicoNodePoolProvider.DiskSelectionStrategy)
	assert.Equal(t, cluster.ID, nodePool.ClusterID)
	assert.True(t, nodePool.ControlPlane)
	assert.True(t, nodePool.TeleportEnabled)
	assert.True(t, nodePool.ProvisioningMonitorDisabled)
	assert.Equal(t, instanceTypeID, nicoNodePoolProvider.InstanceTypeID)
	assert.Equal(t, "250Mi", nodeSet.KubeReservedMemory)
	assert.Equal(t, "250Mi", nodePool.KubeReservedMemory)
}

func TestControlPlaneConfigNodeContext_UsesSnapshotDiskStrategyOnlyForNicoProvider(t *testing.T) {
	node := nicoNode(uuid.New())
	var snapshotCfg db.ControlPlaneConfigSpec
	snapshotCfg.KubeReservedMemory = "250Mi"
	snapshotCfg.SetNico(db.NicoControlPlaneProvider{
		OSProfile:             "os",
		ConfigProfile:         "cfg",
		DiskSelectionStrategy: db.DiskSelectionStrategyLargest,
	})

	tests := []struct {
		providerType db.ProviderType
		wantStrategy db.DiskSelectionStrategy
	}{
		{providerType: db.ProviderTypeNico, wantStrategy: db.DiskSelectionStrategyLargest},
		{providerType: db.ProviderTypeCAPI, wantStrategy: db.DiskSelectionStrategySmallest},
	}
	for _, tt := range tests {
		t.Run(string(tt.providerType), func(t *testing.T) {
			cluster := db.Cluster{
				ID:           uuid.New(),
				ProviderType: tt.providerType,
				ControlPlaneConfig: db.ControlPlaneConfig{
					MaxSurge: 1,
				},
			}
			nodeSet, nodePool := nodes.ControlPlaneConfigNodeContext(cluster, node, snapshotCfg)

			nicoNodeSetProvider, err := nodeSet.NicoProvider()
			require.NoError(t, err)
			assert.Equal(t, tt.wantStrategy, nicoNodeSetProvider.DiskSelectionStrategy)

			nicoNodePoolProvider, err := nodePool.NicoProvider()
			require.NoError(t, err)
			assert.Equal(t, tt.wantStrategy, nicoNodePoolProvider.DiskSelectionStrategy)
			assert.Equal(t, "250Mi", nodeSet.KubeReservedMemory)
			assert.Equal(t, "250Mi", nodePool.KubeReservedMemory)
		})
	}
}

func TestControlPlaneConfigNodeContext_PreservesNicoProviderPayload(t *testing.T) {
	cluster := db.Cluster{ID: uuid.New(), ProviderType: db.ProviderTypeNico}
	node := nicoNode(uuid.New())
	subnetID := uuid.New()
	sshKeyGroupID := uuid.New()

	var snapshotCfg db.ControlPlaneConfigSpec
	snapshotCfg.SetNico(db.NicoControlPlaneProvider{
		OSProfile:             "os",
		ConfigProfile:         "cfg",
		DiskSelectionStrategy: db.DiskSelectionStrategyLargest,
		SubnetIDs:             []uuid.UUID{subnetID},
		SSHKeyGroupIDs:        []uuid.UUID{sshKeyGroupID},
	})

	nodeSet, nodePool := nodes.ControlPlaneConfigNodeContext(cluster, node, snapshotCfg)

	nicoNodeSetProvider, err := nodeSet.NicoProvider()
	require.NoError(t, err)
	assert.Equal(t, "os", nicoNodeSetProvider.OSProfile)
	assert.Equal(t, "cfg", nicoNodeSetProvider.ConfigProfile)
	assert.Equal(t, db.DiskSelectionStrategyLargest, nicoNodeSetProvider.DiskSelectionStrategy)
	assert.Equal(t, []uuid.UUID{subnetID}, nicoNodeSetProvider.SubnetIDs)

	nicoNodePoolProvider, err := nodePool.NicoProvider()
	require.NoError(t, err)
	assert.Equal(t, node.NicoInstanceTypeID(), nicoNodePoolProvider.InstanceTypeID)
	assert.Equal(t, "os", nicoNodePoolProvider.OSProfile)
	assert.Equal(t, "cfg", nicoNodePoolProvider.ConfigProfile)
	assert.Equal(t, db.DiskSelectionStrategyLargest, nicoNodePoolProvider.DiskSelectionStrategy)
	assert.Equal(t, []uuid.UUID{subnetID}, nicoNodePoolProvider.SubnetIDs)
	assert.Equal(t, []uuid.UUID{sshKeyGroupID}, nicoNodePoolProvider.SSHKeyGroupIDs)
}

func TestControlPlaneConfigNodeContext_PreservesCapiProviderPayload(t *testing.T) {
	cluster := db.Cluster{ID: uuid.New(), ProviderType: db.ProviderTypeCAPI}
	node := nicoNode(uuid.New())

	var snapshotCfg db.ControlPlaneConfigSpec
	snapshotCfg.SetCapi(db.CAPIControlPlaneProvider{
		SpireAttestationMethod: db.SpireAttestationMethodJoinToken,
		PreKubeadmCommands:     []string{"echo hi"},
		BootstrapFiles:         []db.CloudInitFile{{Path: "/etc/test", Content: "ok"}},
	})

	nodeSet, nodePool := nodes.ControlPlaneConfigNodeContext(cluster, node, snapshotCfg)

	capiNodeSetProvider, err := nodeSet.CAPIProvider()
	require.NoError(t, err)
	assert.Equal(t, db.SpireAttestationMethodJoinToken, capiNodeSetProvider.SpireAttestationMethod)
	assert.Equal(t, []string{"echo hi"}, capiNodeSetProvider.PreKubeadmCommands)
	assert.Equal(t, []db.CloudInitFile{{Path: "/etc/test", Content: "ok"}}, capiNodeSetProvider.BootstrapFiles)

	capiNodePoolProvider, err := nodePool.CAPIProvider()
	require.NoError(t, err)
	assert.Equal(t, db.SpireAttestationMethodJoinToken, capiNodePoolProvider.SpireAttestationMethod)
	assert.Equal(t, []string{"echo hi"}, capiNodePoolProvider.PreKubeadmCommands)
	assert.Equal(t, []db.CloudInitFile{{Path: "/etc/test", Content: "ok"}}, capiNodePoolProvider.BootstrapFiles)
}

func TestControlPlaneConfigNodeContext_DefaultsDiskStrategy(t *testing.T) {
	node := nicoNode(uuid.New())
	var snapshotCfg db.ControlPlaneConfigSpec
	snapshotCfg.SetNico(db.NicoControlPlaneProvider{
		OSProfile:             "os",
		ConfigProfile:         "cfg",
		DiskSelectionStrategy: "",
	})

	for _, providerType := range []db.ProviderType{db.ProviderTypeNico, db.ProviderTypeCAPI} {
		t.Run(string(providerType), func(t *testing.T) {
			cluster := db.Cluster{ID: uuid.New(), ProviderType: providerType}
			nodeSet, nodePool := nodes.ControlPlaneConfigNodeContext(cluster, node, snapshotCfg)

			nicoNodeSetProvider, err := nodeSet.NicoProvider()
			require.NoError(t, err)
			assert.Equal(t, db.DiskSelectionStrategySmallest, nicoNodeSetProvider.DiskSelectionStrategy)

			nicoNodePoolProvider, err := nodePool.NicoProvider()
			require.NoError(t, err)
			assert.Equal(t, db.DiskSelectionStrategySmallest, nicoNodePoolProvider.DiskSelectionStrategy)
		})
	}
}
