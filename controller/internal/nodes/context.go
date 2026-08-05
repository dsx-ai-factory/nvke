// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package nodes

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/internal/db"
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

	nodeSet, nodePool := ControlPlaneConfigNodeContext(cluster, node, snapshotCfg)
	return nodeSet, nodePool, nil
}

// ControlPlaneConfigNodeContext builds synthetic node set/pool records from cluster
// control plane settings and the resolved node-level template snapshot.
func ControlPlaneConfigNodeContext(cluster db.Cluster, node db.Node, snapshotCfg db.ControlPlaneConfigSpec) (db.NodeSet, db.NodePool) {
	template := json.RawMessage(`{}`)
	if len(snapshotCfg.InfraMachineTemplatePreferences) > 0 {
		template = append(json.RawMessage(nil), snapshotCfg.InfraMachineTemplatePreferences[0]...)
	}
	diskSelectionStrategy := snapshotCfg.DiskSelectionStrategy
	if diskSelectionStrategy == "" {
		// default to smallest disk for control plane
		diskSelectionStrategy = db.DiskSelectionStrategySmallest
	}
	nodeSet := db.NodeSet{
		ID:                         uuid.Nil,
		NodePoolID:                 uuid.Nil,
		OSProfile:                  snapshotCfg.OSProfile,
		DiskSelectionStrategy:      diskSelectionStrategy,
		ConfigProfile:              snapshotCfg.ConfigProfile,
		SystemReservedMemory:       snapshotCfg.SystemReservedMemory,
		SubnetIDs:                  append([]uuid.UUID(nil), snapshotCfg.SubnetIDs...),
		VpcPrefixIDs:               append([]uuid.UUID(nil), snapshotCfg.VpcPrefixIDs...),
		KubernetesVersion:          snapshotCfg.KubernetesVersion,
		TeleportEnabled:            snapshotCfg.TeleportEnabled,
		CapiInfraMachineTemplate:   template,
		CapiBootstrapFiles:         append([]db.CloudInitFile(nil), snapshotCfg.CapiBootstrapFiles...),
		CapiPreKubeadmCommands:     append([]string(nil), snapshotCfg.CapiPreKubeadmCommands...),
		CapiSpireAttestationMethod: snapshotCfg.CapiSpireAttestationMethod,
	}

	nodePool := db.NodePool{
		ID:                          uuid.Nil,
		ClusterID:                   cluster.ID,
		Name:                        "control-plane-config",
		NodeNamePrefix:              "cp",
		SubnetIDs:                   append([]uuid.UUID(nil), snapshotCfg.SubnetIDs...),
		VpcPrefixIDs:                append([]uuid.UUID(nil), snapshotCfg.VpcPrefixIDs...),
		InstanceTypeID:              node.InstanceTypeID,
		SSHKeyGroupIDs:              append([]uuid.UUID(nil), snapshotCfg.SSHKeyGroupIDs...),
		OSProfile:                   snapshotCfg.OSProfile,
		ConfigProfile:               snapshotCfg.ConfigProfile,
		SystemReservedMemory:        snapshotCfg.SystemReservedMemory,
		ControlPlane:                true,
		NodeLabels:                  append([]string(nil), snapshotCfg.NodeLabels...),
		KubernetesVersion:           snapshotCfg.KubernetesVersion,
		MaxSurge:                    cluster.ControlPlaneConfig.MaxSurge,
		TeleportEnabled:             snapshotCfg.TeleportEnabled,
		ProvisioningMonitorDisabled: snapshotCfg.ProvisioningMonitorDisabled,
		CapiInfraMachineTemplate:    template,
		CapiBootstrapFiles:          append([]db.CloudInitFile(nil), snapshotCfg.CapiBootstrapFiles...),
		CapiPreKubeadmCommands:      append([]string(nil), snapshotCfg.CapiPreKubeadmCommands...),
		CapiSpireAttestationMethod:  snapshotCfg.CapiSpireAttestationMethod,
	}

	return nodeSet, nodePool
}
