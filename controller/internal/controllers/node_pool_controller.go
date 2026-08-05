// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package controllers

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
)

// NodePoolController is responsible for reconciling node pools.
type NodePoolController struct {
	cfg    config.Config
	db     db.DB
	logger *slog.Logger
}

func NewNodePoolController(
	cfg config.Config,
	logger *slog.Logger,
	dbInstance db.DB,
) *NodePoolController {
	return &NodePoolController{
		cfg:    cfg,
		db:     dbInstance,
		logger: logger,
	}
}

// Reconcile NodePools, for each NodePool:
// - Create a new Current NodeSet If:
//   - A current NodeSet doesn't exist.
//   - The NodePool's OSProfile or ConfigProfile has changed.
//
// - Distribute the desired count of the NodePool to the NodeSets.
//   - Set the DesiredCount of the current NodeSet to be the DesiredCount of the NodePool
//     less the actual node count of nodes assigned to non-current NodeSets.
//   - Delete non-current NodeSets when their actual node count is 0.
//   - Update the DesiredCount of non-current NodeSets to 0.
func (a *NodePoolController) Reconcile(ctx context.Context, site db.Site) error {
	nodePools, err := a.db.ListNodePools(ctx, db.NodePoolFilters{SiteID: &site.ID})
	if err != nil {
		return fmt.Errorf("failed to list node pools: %w", err)
	}

	for _, nodePool := range nodePools {
		if err := a.reconcileNodePool(ctx, nodePool); err != nil {
			// Log errors but continue processing the other node pools.
			a.logger.Error("Failed to reconcile node pool", "error", err, "nodePoolID", nodePool.ID)
			continue
		}
	}

	return nil
}

// recordNodePoolConfigChange emits an audit record via the outbox capturing the before/after
// config of a node pool change. The no-op RecordNodePoolConfigChange workflow persists the
// snapshot in Temporal history, surviving NodeSet GC. Query via Temporal UI by workflow type
// or ID prefix. prevNodeSet is the zero value when this is the pool's first NodeSet.
func recordNodePoolConfigChange(ctx context.Context, tx db.Tx, prevNodeSet, currentNodeSet db.NodeSet) error {
	var prevNodeSetID *uuid.UUID
	if prevNodeSet.ID != uuid.Nil {
		prevNodeSetID = &prevNodeSet.ID
	}

	auditID := constants.RecordNodePoolConfigChangeWorkflowPrefix +
		currentNodeSet.NodePoolID.String() + "-" + currentNodeSet.ID.String()
	if _, err := tx.CreateWorkflowOutboxEntry(ctx, constants.RecordNodePoolConfigChangeWorkflowType, auditID, map[string]interface{}{
		"nodePoolID":    currentNodeSet.NodePoolID,
		"newNodeSetID":  currentNodeSet.ID,
		"prevNodeSetID": prevNodeSetID,

		"oldOSProfile": prevNodeSet.OSProfile,
		"newOSProfile": currentNodeSet.OSProfile,

		"oldConfigProfile": prevNodeSet.ConfigProfile,
		"newConfigProfile": currentNodeSet.ConfigProfile,

		"oldKubernetesVersion": prevNodeSet.KubernetesVersion,
		"newKubernetesVersion": currentNodeSet.KubernetesVersion,

		"oldSystemReservedMemory": prevNodeSet.SystemReservedMemory,
		"newSystemReservedMemory": currentNodeSet.SystemReservedMemory,

		"oldDiskSelectionStrategy": prevNodeSet.DiskSelectionStrategy,
		"newDiskSelectionStrategy": currentNodeSet.DiskSelectionStrategy,

		"oldCapiSpireAttestationMethod": prevNodeSet.CapiSpireAttestationMethod,
		"newCapiSpireAttestationMethod": currentNodeSet.CapiSpireAttestationMethod,

		"oldTeleportEnabled": prevNodeSet.TeleportEnabled,
		"newTeleportEnabled": currentNodeSet.TeleportEnabled,

		"oldSubnetIDs": prevNodeSet.SubnetIDs,
		"newSubnetIDs": currentNodeSet.SubnetIDs,

		"oldVpcPrefixIDs": prevNodeSet.VpcPrefixIDs,
		"newVpcPrefixIDs": currentNodeSet.VpcPrefixIDs,

		"oldCapiPreKubeadmCommands": prevNodeSet.CapiPreKubeadmCommands,
		"newCapiPreKubeadmCommands": currentNodeSet.CapiPreKubeadmCommands,
	}); err != nil {
		return fmt.Errorf("failed to create config change audit entry: %w", err)
	}
	return nil
}

func (a *NodePoolController) reconcileNodePool(ctx context.Context, nodePool db.NodePool) error {
	return a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		logger := a.logger.With(
			"nodePoolID", nodePool.ID,
			"nodePoolDesiredCount", nodePool.DesiredCount,
			"maxSurge", nodePool.MaxSurge,
		)

		var err error

		if nodePool.AutoProvisionDisabled {
			logger.Debug("Node pool has auto-provision disabled, skipping node pool reconciliation")
			return nil
		}

		// If the node pool is deleting, skip reconciliation.
		if nodePool.Conditions.Deleting.Status == db.ConditionStatusTrue {
			logger.Debug("Node pool is deleting, skipping node pool reconciliation")
			return nil
		}

		var currentNodeSet db.NodeSet
		if nodePool.CurrentNodeSetID != nil {
			currentNodeSet, err = tx.GetNodeSet(ctx, *nodePool.CurrentNodeSetID)
			if err != nil {
				return fmt.Errorf("failed to get current node set: %w", err)
			}
		}

		// Create a new NodeSet if the NodePool's configuration has changed.
		configChanged := nodePool.CurrentNodeSetID == nil ||
			nodePool.OSProfile != currentNodeSet.OSProfile ||
			nodePool.KubernetesVersion != currentNodeSet.KubernetesVersion ||
			nodePool.ConfigProfile != currentNodeSet.ConfigProfile ||
			!bytes.Equal(nodePool.CapiInfraMachineTemplate, currentNodeSet.CapiInfraMachineTemplate) ||
			nodePool.CapiSpireAttestationMethod != currentNodeSet.CapiSpireAttestationMethod ||
			!cloudInitFilesEqual(nodePool.CapiBootstrapFiles, currentNodeSet.CapiBootstrapFiles) ||
			!slices.Equal(nodePool.CapiPreKubeadmCommands, currentNodeSet.CapiPreKubeadmCommands) ||
			nodePool.SystemReservedMemory != currentNodeSet.SystemReservedMemory ||
			nodePool.TeleportEnabled != currentNodeSet.TeleportEnabled ||
			nodePool.DiskSelectionStrategy != currentNodeSet.DiskSelectionStrategy ||
			!uuidSlicesSetEqual(nodePool.SubnetIDs, currentNodeSet.SubnetIDs) ||
			!uuidSlicesSetEqual(nodePool.VpcPrefixIDs, currentNodeSet.VpcPrefixIDs)

		if configChanged {
			// Capture previous state before currentNodeSet is overwritten below.
			prevNodeSet := currentNodeSet

			currentNodeSet, err = tx.CreateNodeSet(ctx, &db.NodeSet{
				ID:                         uuid.New(),
				NodePoolID:                 nodePool.ID,
				OSProfile:                  nodePool.OSProfile,
				ConfigProfile:              nodePool.ConfigProfile,
				CapiInfraMachineTemplate:   nodePool.CapiInfraMachineTemplate,
				CapiSpireAttestationMethod: nodePool.CapiSpireAttestationMethod,
				CapiBootstrapFiles:         slices.Clone(nodePool.CapiBootstrapFiles),
				CapiPreKubeadmCommands:     slices.Clone(nodePool.CapiPreKubeadmCommands),
				KubernetesVersion:          nodePool.KubernetesVersion,
				SystemReservedMemory:       nodePool.SystemReservedMemory,
				TeleportEnabled:            nodePool.TeleportEnabled,
				DiskSelectionStrategy:      nodePool.DiskSelectionStrategy,
				SubnetIDs:                  slices.Clone(nodePool.SubnetIDs),
				VpcPrefixIDs:               slices.Clone(nodePool.VpcPrefixIDs),
			})
			if err != nil {
				return fmt.Errorf("failed to create node set: %w", err)
			}

			// Update the node pool's current node set ID to the new node set
			nodePool.CurrentNodeSetID = &currentNodeSet.ID
			nodePool, err = tx.UpdateNodePool(ctx, &nodePool)
			if err != nil {
				return fmt.Errorf("failed to update node pool: %w", err)
			}
			logger.Info("Created new current node set",
				"nodeSetID", currentNodeSet.ID,
				"desiredCount", 0)

			if err := recordNodePoolConfigChange(ctx, tx, prevNodeSet, currentNodeSet); err != nil {
				return err
			}
		}

		nodeSets, err := tx.ListNodeSets(ctx, db.NodeSetFilters{NodePoolID: &nodePool.ID})
		if err != nil {
			return fmt.Errorf("failed to list node sets: %w", err)
		}

		// Set the desiredNodeCount of every non-current NodeSet to 0.
		// And count the number of nodes in non-current node sets.
		nodesInNonCurrentNodeSets := 0
		for _, nodeSet := range nodeSets {
			if nodeSet.ID == currentNodeSet.ID {
				// This is the current node set, so we don't need to do anything other than reset the currentNodeSet.
				currentNodeSet = nodeSet
				continue
			}

			// If we wanted to be strict about NodePoolController only interacting with NodeSets we could
			// Add an ActualCount field to NodeSet, and have NodeSet Controller update it (similar to status in k8s)
			//    - However that is essentially Len(nodes where NodeSetID = xx).
			nodeSetNodes, err := tx.ListNodes(ctx, db.NodeFilters{NodeSetID: &nodeSet.ID})
			if err != nil {
				return fmt.Errorf("failed to list nodes: %w", err)
			}

			nodesInNonCurrentNodeSets += len(nodeSetNodes)

			// If there are no nodes in the non-current node set, delete it.
			// Else set the desired count to 0.
			if len(nodeSetNodes) == 0 {
				if err := tx.DeleteNodeSet(ctx, nodeSet.ID); err != nil {
					return fmt.Errorf("failed to delete non-current node set: %w", err)
				}
				logger.Info("Deleted non-current node set", "nodeSetID", nodeSet.ID)
			} else {
				nodeSet.DesiredCount = 0
				if _, err := tx.UpdateNodeSet(ctx, &nodeSet); err != nil {
					return fmt.Errorf("failed to update non-current node set: %w", err)
				}
				logger.Debug("Updated non-current node set desired count", "nodeSetID", nodeSet.ID, "desiredCount", nodeSet.DesiredCount)
			}
		}

		// Calculate surge nodes for migration
		// Only surge when there are deprecated nodes to migrate, enabling faster rollout
		// Cap surge to the number of deprecated nodes (no point surging more than what we're migrating)
		surgeNodes := min(nodePool.MaxSurge, nodesInNonCurrentNodeSets)

		// Set the desired count of the current nodeSet to be the desired count of the node pool
		// less the actual node count of nodes assigned to non-current NodeSets,
		// plus surge nodes for migration.
		//
		// Even though the desired count for a non-current node set is 0,
		// we still need to account for the actual nodes that exist in the non-current node sets
		// so that we don't over-provision the current node set.
		// This will be re-calculated as nodes are de-provisioned or deleted from the non-current node sets.
		currentNodeSetDesiredCount := max(0, nodePool.DesiredCount-nodesInNonCurrentNodeSets+surgeNodes)

		// Cap the desired count to never exceed nodePool.DesiredCount
		// This prevents creating extra surge nodes after all non-current nodes are deleted
		// if there are any race conditions, the current node set desired count will be adjusted to the node pool desired count.
		// Example: nodePool.DesiredCount=2, nodesInNonCurrentNodeSets=0, surge=1 → cap 3 to 2
		currentNodeSetDesiredCount = min(currentNodeSetDesiredCount, nodePool.DesiredCount)

		if currentNodeSet.DesiredCount != currentNodeSetDesiredCount {
			currentNodeSet.DesiredCount = currentNodeSetDesiredCount
			if _, err := tx.UpdateNodeSet(ctx, &currentNodeSet); err != nil {
				return fmt.Errorf("failed to update current node set: %w", err)
			}

			logger.Info("Updated current node set desired count", "nodeSetID", currentNodeSet.ID, "desiredCount", currentNodeSet.DesiredCount, "nodesInNonCurrentNodeSets", nodesInNonCurrentNodeSets)
		}

		return nil
	})
}

// uuidSlicesSetEqual reports whether a and b contain the same set of UUIDs (order-independent).
// nil and empty slice are treated as the same set.
func uuidSlicesSetEqual(a, b []uuid.UUID) bool {
	toSet := func(s []uuid.UUID) map[uuid.UUID]struct{} {
		out := make(map[uuid.UUID]struct{})
		for _, id := range s {
			out[id] = struct{}{}
		}
		return out
	}
	setA := toSet(a)
	setB := toSet(b)
	if len(setA) != len(setB) {
		return false
	}
	for id := range setA {
		if _, ok := setB[id]; !ok {
			return false
		}
	}
	return true
}

func cloudInitFilesEqual(a, b []db.CloudInitFile) bool {
	return slices.EqualFunc(a, b, cloudInitFileEqual)
}

func cloudInitFileEqual(a, b db.CloudInitFile) bool {
	return a.Path == b.Path &&
		a.Owner == b.Owner &&
		a.Permissions == b.Permissions &&
		a.Content == b.Content
}
