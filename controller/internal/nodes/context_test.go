// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package nodes_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/nodes"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestGetNodeContext_NoNodePoolControlPlane(t *testing.T) {
	ctx := context.Background()
	bunDB := testutil.SetupDB(ctx, t,
		(*db.Site)(nil),
		(*db.Cluster)(nil),
		(*db.ControlPlaneConfiguration)(nil),
		(*db.Node)(nil),
	)
	database := db.NewDB(bunDB)

	site := db.Site{ID: uuid.New(), VIPRanges: []string{}}
	_, err := database.RegisterSite(ctx, &site)
	require.NoError(t, err)

	cluster := db.Cluster{
		ID:                uuid.New(),
		SiteID:            site.ID,
		Name:              "cp-cluster",
		OrgID:             "test-org",
		VpcID:             uuid.New(),
		KubernetesVersion: "1.31.0",
		TeleportEnabled:   true,
	}
	_, err = database.CreateCluster(ctx, &cluster)
	require.NoError(t, err)

	snapshot, err := database.CreateControlPlaneConfiguration(ctx, &db.ControlPlaneConfiguration{
		ClusterID: cluster.ID,
		Config: db.ControlPlaneConfigSpec{
			OSProfile:                   "os",
			DiskSelectionStrategy:       "",
			ConfigProfile:               "cfg",
			TeleportEnabled:             true,
			ProvisioningMonitorDisabled: true,
		},
	})
	require.NoError(t, err)
	cluster.ControlPlaneConfig.ConfigurationID = snapshot.ID
	_, err = database.UpdateCluster(ctx, &cluster)
	require.NoError(t, err)

	instanceTypeID := uuid.New()
	node := db.Node{
		ID:             uuid.New(),
		ClusterID:      cluster.ID,
		ControlPlane:   true,
		InstanceTypeID: instanceTypeID,
		Name:           "cp-1",
		IP:             "10.0.0.1",
	}
	_, err = database.CreateNode(ctx, &node)
	require.NoError(t, err)

	nodeSet, nodePool, err := nodes.GetNodeContext(ctx, database, node)
	require.NoError(t, err)
	assert.Equal(t, uuid.Nil, nodeSet.ID)
	assert.Equal(t, uuid.Nil, nodePool.ID)
	assert.Equal(t, db.DiskSelectionStrategySmallest, nodeSet.DiskSelectionStrategy)
	assert.Equal(t, cluster.ID, nodePool.ClusterID)
	assert.True(t, nodePool.ControlPlane)
	assert.True(t, nodePool.TeleportEnabled)
	assert.True(t, nodePool.ProvisioningMonitorDisabled)
	assert.Equal(t, instanceTypeID, nodePool.InstanceTypeID)
}

func TestControlPlaneConfigNodeContext_UsesSnapshotDiskStrategy(t *testing.T) {
	cluster := db.Cluster{
		ID: uuid.New(),
		ControlPlaneConfig: db.ControlPlaneConfig{
			MaxSurge: 1,
		},
	}
	node := db.Node{
		InstanceTypeID: uuid.New(),
	}
	snapshotCfg := db.ControlPlaneConfigSpec{
		OSProfile:             "os",
		ConfigProfile:         "cfg",
		DiskSelectionStrategy: db.DiskSelectionStrategyLargest,
	}

	nodeSet, _ := nodes.ControlPlaneConfigNodeContext(cluster, node, snapshotCfg)
	assert.Equal(t, db.DiskSelectionStrategyLargest, nodeSet.DiskSelectionStrategy)
}
