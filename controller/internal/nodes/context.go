// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodes

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
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
	if snapshotCfg.Capi != nil && len(snapshotCfg.Capi.InfraMachineTemplatePreferences) > 0 {
		template = append(json.RawMessage(nil), snapshotCfg.Capi.InfraMachineTemplatePreferences[0]...)
	}

	// default to smallest disk for control plane
	nicoDiskSelectionStrategy := db.DiskSelectionStrategySmallest
	if cluster.ProviderType == db.ProviderTypeNico &&
		snapshotCfg.Nico != nil &&
		snapshotCfg.Nico.DiskSelectionStrategy != "" {
		nicoDiskSelectionStrategy = snapshotCfg.Nico.DiskSelectionStrategy
	}

	nodeSet := db.NodeSet{
		ID:                   uuid.Nil,
		NodePoolID:           uuid.Nil,
		SystemReservedMemory: snapshotCfg.SystemReservedMemory,
		KubeReservedMemory:   snapshotCfg.KubeReservedMemory,
		KubernetesVersion:    snapshotCfg.KubernetesVersion,
		TeleportEnabled:      snapshotCfg.TeleportEnabled,
	}

	// Populate provider payloads from the snapshot, mirroring the nodePool population below,
	// so downstream strict-getter reads on this synthetic NodeSet succeed.
	if snapshotCfg.Nico != nil {
		nodeSet.SetNico(db.NicoNodePoolProvider{
			SubnetIDs:             append([]uuid.UUID(nil), snapshotCfg.Nico.SubnetIDs...),
			VpcPrefixIDs:          append([]uuid.UUID(nil), snapshotCfg.Nico.VpcPrefixIDs...),
			OSProfile:             snapshotCfg.Nico.OSProfile,
			ConfigProfile:         snapshotCfg.Nico.ConfigProfile,
			DiskSelectionStrategy: nicoDiskSelectionStrategy,
		})
	}
	if snapshotCfg.Capi != nil {
		nodeSet.SetCapi(db.CAPINodePoolProvider{
			InfraMachineTemplate:   template,
			SpireAttestationMethod: snapshotCfg.Capi.SpireAttestationMethod,
			BootstrapFiles:         append([]db.CloudInitFile(nil), snapshotCfg.Capi.BootstrapFiles...),
			PreKubeadmCommands:     append([]string(nil), snapshotCfg.Capi.PreKubeadmCommands...),
		})
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

	// Populate provider payloads from the snapshot, so downstream strict-getter
	// reads on this synthetic NodePool succeed.
	if snapshotCfg.Nico != nil {
		nodePool.SetNico(db.NicoNodePoolProvider{
			InstanceTypeID:        node.NicoInstanceTypeID(),
			SubnetIDs:             append([]uuid.UUID(nil), snapshotCfg.Nico.SubnetIDs...),
			VpcPrefixIDs:          append([]uuid.UUID(nil), snapshotCfg.Nico.VpcPrefixIDs...),
			SSHKeyGroupIDs:        append([]uuid.UUID(nil), snapshotCfg.Nico.SSHKeyGroupIDs...),
			OSProfile:             snapshotCfg.Nico.OSProfile,
			ConfigProfile:         snapshotCfg.Nico.ConfigProfile,
			DiskSelectionStrategy: nicoDiskSelectionStrategy,
		})
	}
	if snapshotCfg.Capi != nil {
		nodePool.SetCapi(db.CAPINodePoolProvider{
			InfraMachineTemplate:   template,
			SpireAttestationMethod: snapshotCfg.Capi.SpireAttestationMethod,
			BootstrapFiles:         append([]db.CloudInitFile(nil), snapshotCfg.Capi.BootstrapFiles...),
			PreKubeadmCommands:     append([]string(nil), snapshotCfg.Capi.PreKubeadmCommands...),
		})
	}

	return nodeSet, nodePool
}
