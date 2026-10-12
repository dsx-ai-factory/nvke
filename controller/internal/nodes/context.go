// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodes

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// GetNodeContext resolves the effective node set and node pool for a node.
// Worker nodes must have a node set; control-plane nodes without a pool/set use cluster control_plane_config.
func GetNodeContext(ctx context.Context, queries db.Queries, node db.Node) (db.NodeSet, db.NodePool, error) {
	if node.IsManagedByNodePool() {
		nodeSet, err := queries.GetNodeSet(ctx, node.NodeSetID)
		if err != nil {
			return db.NodeSet{}, db.NodePool{}, fmt.Errorf("failed to get node set: %w", err)
		}

		nodePool, err := queries.GetNodePool(ctx, nodeSet.NodePoolID)
		if err != nil {
			return db.NodeSet{}, db.NodePool{}, fmt.Errorf("failed to get node pool: %w", err)
		}

		return nodeSet, nodePool, nil
	}

	if !node.ControlPlane {
		return db.NodeSet{}, db.NodePool{}, fmt.Errorf("worker node %s must be associated with nodepool/nodeset", node.ID)
	}

	cluster, err := queries.GetCluster(ctx, node.ClusterID)
	if err != nil {
		return db.NodeSet{}, db.NodePool{}, fmt.Errorf("failed to get cluster: %w", err)
	}

	snapshotCfg := cluster.ControlPlaneConfig.ControlPlaneConfigSpec
	// if the node has a control plane configuration, use it to override the cluster control plane config
	// otherwise, use the cluster control plane config (for older nodes that don't have a control plane configuration)
	if node.ControlPlaneConfigurationID != uuid.Nil {
		row, err := queries.GetControlPlaneConfiguration(ctx, node.ControlPlaneConfigurationID)
		if err != nil {
			return db.NodeSet{}, db.NodePool{}, fmt.Errorf("failed to get node configuration snapshot: %w", err)
		}
		snapshotCfg = row.Config
	}

	return ControlPlaneConfigNodeContext(cluster, node, snapshotCfg)
}

// ControlPlaneConfigNodeContext builds synthetic node set/pool records from
// cluster control plane settings and the resolved node-level template
// snapshot. Provider-specific payload construction is delegated to
// clients.ControlPlaneConfigNodeProvider so this stays provider-agnostic.
func ControlPlaneConfigNodeContext(cluster db.Cluster, node db.Node, snapshotCfg db.ControlPlaneConfigSpec) (db.NodeSet, db.NodePool, error) {
	nodeSet := db.NodeSet{
		ID:                   uuid.Nil,
		NodePoolID:           uuid.Nil,
		SystemReservedMemory: snapshotCfg.SystemReservedMemory,
		KubeReservedMemory:   snapshotCfg.KubeReservedMemory,
		KubernetesVersion:    snapshotCfg.KubernetesVersion,
		TeleportEnabled:      snapshotCfg.TeleportEnabled,
	}
	nodePool := db.NodePool{
		ID:                          uuid.Nil,
		ClusterID:                   cluster.ID,
		Name:                        "control-plane-config",
		NodeNamePrefix:              "cp",
		ControlPlane:                true,
		NodeLabels:                  db.CloneStringMap(snapshotCfg.NodeLabels),
		KubernetesVersion:           snapshotCfg.KubernetesVersion,
		MaxSurge:                    cluster.ControlPlaneConfig.MaxSurge,
		SystemReservedMemory:        snapshotCfg.SystemReservedMemory,
		KubeReservedMemory:          snapshotCfg.KubeReservedMemory,
		TeleportEnabled:             snapshotCfg.TeleportEnabled,
		ProvisioningMonitorDisabled: snapshotCfg.ProvisioningMonitorDisabled,
	}

	provider, err := clients.GetControlPlaneConfigNodeProvider(cluster)
	if err != nil {
		return db.NodeSet{}, db.NodePool{}, err
	}
	provider.ApplyControlPlaneConfigPayloads(node, snapshotCfg, &nodeSet, &nodePool)

	return nodeSet, nodePool, nil
}
