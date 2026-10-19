// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// ApplyControlPlaneConfigPayloads sets nodeSet's and nodePool's LaunchLayer
// payload from snapshotCfg.LaunchLayer. Control-plane nodes are still
// LaunchLayer machines with the same field set as a node pool, so both get
// an identical copy — there's no per-node field (like NICo's pinned
// instance type) to vary between them.
func (*Client) ApplyControlPlaneConfigPayloads(_ db.Node, snapshotCfg db.ControlPlaneConfigSpec, nodeSet *db.NodeSet, nodePool *db.NodePool) {
	if snapshotCfg.LaunchLayer == nil {
		return
	}

	provider := db.LaunchLayerNodePoolProvider{
		InstanceType:        snapshotCfg.LaunchLayer.InstanceType,
		HostOSProfile:       snapshotCfg.LaunchLayer.HostOSProfile,
		VMProfile:           snapshotCfg.LaunchLayer.VMProfile,
		FulfillmentStrategy: snapshotCfg.LaunchLayer.FulfillmentStrategy,
		SecurityGroupRefs:   snapshotCfg.LaunchLayer.SecurityGroupRefs,
	}
	nodeSet.SetLaunchLayer(provider)
	nodePool.SetLaunchLayer(provider)
}
