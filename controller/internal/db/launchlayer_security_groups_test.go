// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestLaunchLayerSecurityGroups_CloneAndEquality(t *testing.T) {
	provider := db.LaunchLayerNodePoolProvider{
		InstanceType: "bare-metal",
		SecurityGroups: db.LaunchLayerSecurityGroups{
			PrimaryNetworkInterface: []string{"external-primary"},
			GPUNetworkInterface:     []string{"external-gpu"},
		},
	}
	var pool db.NodePool
	pool.SetLaunchLayer(provider)
	var nodeSet db.NodeSet
	nodeSet.SetLaunchLayer(provider)
	var spec db.ControlPlaneConfigSpec
	spec.SetLaunchLayer(db.LaunchLayerControlPlaneProvider{
		InstanceType: provider.InstanceType, SecurityGroups: provider.SecurityGroups,
	})
	provider.SecurityGroups.PrimaryNetworkInterface[0] = "changed-primary"
	provider.SecurityGroups.GPUNetworkInterface[0] = "changed-gpu"

	for _, groups := range []db.LaunchLayerSecurityGroups{
		pool.LaunchLayer.SecurityGroups, nodeSet.LaunchLayer.SecurityGroups, spec.LaunchLayer.SecurityGroups,
	} {
		require.Equal(t, []string{"external-primary"}, groups.PrimaryNetworkInterface)
		require.Equal(t, []string{"external-gpu"}, groups.GPUNetworkInterface)
	}
	poolCopy, err := pool.LaunchLayerProvider()
	require.NoError(t, err)
	nodeSetCopy, err := nodeSet.LaunchLayerProvider()
	require.NoError(t, err)
	cpCopy, err := spec.LaunchLayerProvider()
	require.NoError(t, err)
	for _, groups := range []db.LaunchLayerSecurityGroups{poolCopy.SecurityGroups, nodeSetCopy.SecurityGroups, cpCopy.SecurityGroups} {
		groups.PrimaryNetworkInterface[0] = "mutated-primary"
		groups.GPUNetworkInterface[0] = "mutated-gpu"
	}
	require.Equal(t, "external-primary", pool.LaunchLayer.SecurityGroups.PrimaryNetworkInterface[0])
	require.Equal(t, "external-gpu", nodeSet.LaunchLayer.SecurityGroups.GPUNetworkInterface[0])
	require.Equal(t, "external-primary", spec.LaunchLayer.SecurityGroups.PrimaryNetworkInterface[0])
	require.Equal(t, "external-gpu", spec.LaunchLayer.SecurityGroups.GPUNetworkInterface[0])

	pool.CurrentNodeSetID = new(uuid.UUID)
	require.False(t, db.NodePoolNeedsNewNodeSet(pool, nodeSet))
	for _, iface := range []string{"primary", "gpu"} {
		t.Run(iface, func(t *testing.T) {
			changed, err := pool.LaunchLayerProvider()
			require.NoError(t, err)
			changedCP, err := spec.LaunchLayerProvider()
			require.NoError(t, err)
			if iface == "primary" {
				changed.SecurityGroups.PrimaryNetworkInterface[0] = "replacement"
				changedCP.SecurityGroups.PrimaryNetworkInterface[0] = "replacement"
			} else {
				changed.SecurityGroups.GPUNetworkInterface[0] = "replacement"
				changedCP.SecurityGroups.GPUNetworkInterface[0] = "replacement"
			}
			require.False(t, pool.LaunchLayer.Equal(changed))
			changedPool := pool
			changedPool.SetLaunchLayer(changed)
			require.True(t, db.NodePoolNeedsNewNodeSet(changedPool, nodeSet))
			changedSpec := spec
			changedSpec.SetLaunchLayer(changedCP)
			require.False(t, spec.Equal(changedSpec))
		})
	}
}

func TestLaunchLayerSecurityGroups_LegacyJSON(t *testing.T) {
	const legacy = `{"instance_type":"bare-metal","security_group_refs":{"primary_network_interface":["managed"]}}`
	var provider db.LaunchLayerNodePoolProvider
	require.NoError(t, json.Unmarshal([]byte(legacy), &provider))
	require.Equal(t, []string{"managed"}, provider.SecurityGroupRefs.PrimaryNetworkInterface)
	require.Empty(t, provider.SecurityGroups)
	withEmpty := provider
	withEmpty.SecurityGroups = db.LaunchLayerSecurityGroups{PrimaryNetworkInterface: []string{}, GPUNetworkInterface: []string{}}
	require.True(t, provider.Equal(withEmpty))

	var oldCP db.LaunchLayerControlPlaneProvider
	require.NoError(t, json.Unmarshal([]byte(legacy), &oldCP))
	emptyCP := oldCP
	emptyCP.SecurityGroups = withEmpty.SecurityGroups
	require.True(t, (db.ControlPlaneConfigSpec{LaunchLayer: &oldCP}).Equal(db.ControlPlaneConfigSpec{LaunchLayer: &emptyCP}))
}
