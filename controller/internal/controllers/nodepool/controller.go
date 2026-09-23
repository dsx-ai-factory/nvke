// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodepool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile/filters"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// Controller is responsible for reconciling node pools.
type Controller struct {
	cfg    config.Config
	db     *dbwrapper.DB
	logger *slog.Logger
}

func NewController(
	cfg config.Config,
	logger *slog.Logger,
	dbInstance *dbwrapper.DB,
) *Controller {
	return &Controller{
		cfg:    cfg,
		db:     dbInstance,
		logger: logger,
	}
}

func (a *Controller) SetupWithManager(mgr manager.Manager) error {
	return dbreconcile.SetupController(
		mgr,
		"node-pool-controller",
		a,
		dbwrapper.NewNodePoolSource(a.db, dbfilters.NodePool{}),
		dbwrapper.NewNodeSource(a.db, dbfilters.Node{}, nodePoolKeysForNode),
	)
}

func nodePoolKeysForNode(_ context.Context, node db.Node) ([]dbreconcile.Key, error) {
	if node.NodePoolID == uuid.Nil {
		return nil, nil
	}
	return []dbreconcile.Key{dbreconcile.NodePoolKey(node.NodePoolID)}, nil
}

// Reconcile a NodePool:
// - Create a new Current NodeSet If:
//   - A current NodeSet doesn't exist.
//   - The NodePool's OSProfile or ConfigProfile has changed.
//
// - Distribute the desired count of the NodePool to the NodeSets.
//   - Set the DesiredCount of the current NodeSet to be the DesiredCount of the NodePool
//     less the actual node count of nodes assigned to non-current NodeSets.
//   - Delete non-current NodeSets when their actual node count is 0.
//   - Update the DesiredCount of non-current NodeSets to 0.
func (a *Controller) Reconcile(ctx context.Context, key dbreconcile.Key) (reconcile.Result, error) {
	err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		nodePool, err := tx.GetNodePoolForUpdate(ctx, key.ID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				a.logger.Debug("Node pool not found, skipping reconciliation", "nodePoolID", key.ID)
				return nil
			}
			return fmt.Errorf("failed to get node pool: %w", err)
		}

		return a.reconcileNodePool(ctx, tx, nodePool)
	})
	return reconcile.Result{}, err
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

	// Best-effort reads for audit only: missing payloads yield zero values rather than
	// failing NodeSet creation. Exactly one of Nico/CAPI is set per NodeSet in practice.
	prevNicoNodeSetProvider, _ := prevNodeSet.NicoProvider()
	currentNicoNodeSetProvider, _ := currentNodeSet.NicoProvider()
	prevCapiNodeSetProvider, _ := prevNodeSet.CAPIProvider()
	currentCapiNodeSetProvider, _ := currentNodeSet.CAPIProvider()

	auditID := constants.RecordNodePoolConfigChangeWorkflowPrefix +
		currentNodeSet.NodePoolID.String() + "-" + currentNodeSet.ID.String()
	if _, err := tx.CreateWorkflowOutboxEntry(ctx, constants.RecordNodePoolConfigChangeWorkflowType, auditID, map[string]interface{}{
		"nodePoolID":    currentNodeSet.NodePoolID,
		"newNodeSetID":  currentNodeSet.ID,
		"prevNodeSetID": prevNodeSetID,

		"oldOSProfile": prevNicoNodeSetProvider.OSProfile,
		"newOSProfile": currentNicoNodeSetProvider.OSProfile,

		"oldConfigProfile": prevNicoNodeSetProvider.ConfigProfile,
		"newConfigProfile": currentNicoNodeSetProvider.ConfigProfile,

		"oldKubernetesVersion": prevNodeSet.KubernetesVersion,
		"newKubernetesVersion": currentNodeSet.KubernetesVersion,

		"oldSystemReservedMemory": prevNodeSet.SystemReservedMemory,
		"newSystemReservedMemory": currentNodeSet.SystemReservedMemory,
		"oldKubeReservedMemory":   prevNodeSet.KubeReservedMemory,
		"newKubeReservedMemory":   currentNodeSet.KubeReservedMemory,

		"oldDiskSelectionStrategy": prevNicoNodeSetProvider.DiskSelectionStrategy,
		"newDiskSelectionStrategy": currentNicoNodeSetProvider.DiskSelectionStrategy,

		"oldCapiSpireAttestationMethod": prevCapiNodeSetProvider.SpireAttestationMethod,
		"newCapiSpireAttestationMethod": currentCapiNodeSetProvider.SpireAttestationMethod,

		"oldTeleportEnabled": prevNodeSet.TeleportEnabled,
		"newTeleportEnabled": currentNodeSet.TeleportEnabled,

		"oldSubnetIDs": prevNicoNodeSetProvider.SubnetIDs,
		"newSubnetIDs": currentNicoNodeSetProvider.SubnetIDs,

		"oldVpcPrefixIDs": prevNicoNodeSetProvider.VpcPrefixIDs,
		"newVpcPrefixIDs": currentNicoNodeSetProvider.VpcPrefixIDs,

		"oldCapiPreKubeadmCommands": prevCapiNodeSetProvider.PreKubeadmCommands,
		"newCapiPreKubeadmCommands": currentCapiNodeSetProvider.PreKubeadmCommands,
	}); err != nil {
		return fmt.Errorf("failed to create config change audit entry: %w", err)
	}
	return nil
}

func (a *Controller) reconcileNodePool(ctx context.Context, tx db.Tx, nodePool db.NodePool) error {
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

	// Nico and CAPI are mutually exclusive — exactly one is set per pool.
	switch {
	case nodePool.Nico != nil:
		nodePool, currentNodeSet, err = a.createNicoNodeSetIfChanged(ctx, tx, nodePool, currentNodeSet, logger)
	case nodePool.Capi != nil:
		nodePool, currentNodeSet, err = a.createCAPINodeSetIfChanged(ctx, tx, nodePool, currentNodeSet, logger)
	default:
		err = fmt.Errorf("node pool %s has no provider payload: expected exactly one of Nico or CAPI to be set", nodePool.ID)
	}
	if err != nil {
		return err
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
	// Example: nodePool.DesiredCount=2, nodesInNonCurrentNodeSets=0 → cap 3 to 2
	currentNodeSetDesiredCount = min(currentNodeSetDesiredCount, nodePool.DesiredCount)

	if currentNodeSet.DesiredCount != currentNodeSetDesiredCount {
		currentNodeSet.DesiredCount = currentNodeSetDesiredCount
		if _, err := tx.UpdateNodeSet(ctx, &currentNodeSet); err != nil {
			return fmt.Errorf("failed to update current node set: %w", err)
		}

		logger.Info("Updated current node set desired count", "nodeSetID", currentNodeSet.ID, "desiredCount", currentNodeSet.DesiredCount, "nodesInNonCurrentNodeSets", nodesInNonCurrentNodeSets)
	}

	return nil
}

// createNicoNodeSetIfChanged compares the pool's Nico provider and common fields
// against the current NodeSet, and creates a new NodeSet when anything has changed.
func (a *Controller) createNicoNodeSetIfChanged(
	ctx context.Context, tx db.Tx,
	nodePool db.NodePool, currentNodeSet db.NodeSet,
	logger *slog.Logger,
) (db.NodePool, db.NodeSet, error) {
	nicoNodePoolProvider, _ := nodePool.NicoProvider()
	currentNicoNodePoolProvider, _ := currentNodeSet.NicoProvider()

	configChanged := nodePool.CurrentNodeSetID == nil ||
		nodePool.KubernetesVersion != currentNodeSet.KubernetesVersion ||
		nodePool.SystemReservedMemory != currentNodeSet.SystemReservedMemory ||
		nodePool.KubeReservedMemory != currentNodeSet.KubeReservedMemory ||
		nodePool.TeleportEnabled != currentNodeSet.TeleportEnabled ||
		!nicoNodePoolProvider.Equal(currentNicoNodePoolProvider)
	if !configChanged {
		return nodePool, currentNodeSet, nil
	}
	newNodeSet := buildBaseNodeSet(nodePool)
	newNodeSet.SetNico(nicoNodePoolProvider)
	return applyNewNodeSet(ctx, tx, nodePool, currentNodeSet, newNodeSet, logger)
}

// createCAPINodeSetIfChanged compares the pool's CAPI provider and common fields
// against the current NodeSet, and creates a new NodeSet when anything has changed.
func (a *Controller) createCAPINodeSetIfChanged(
	ctx context.Context, tx db.Tx,
	nodePool db.NodePool, currentNodeSet db.NodeSet,
	logger *slog.Logger,
) (db.NodePool, db.NodeSet, error) {
	capiNodePoolProvider, _ := nodePool.CAPIProvider()
	currentCapiNodePoolProvider, _ := currentNodeSet.CAPIProvider()

	configChanged := nodePool.CurrentNodeSetID == nil ||
		nodePool.KubernetesVersion != currentNodeSet.KubernetesVersion ||
		nodePool.SystemReservedMemory != currentNodeSet.SystemReservedMemory ||
		nodePool.KubeReservedMemory != currentNodeSet.KubeReservedMemory ||
		nodePool.TeleportEnabled != currentNodeSet.TeleportEnabled ||
		!capiNodePoolProvider.Equal(currentCapiNodePoolProvider)
	if !configChanged {
		return nodePool, currentNodeSet, nil
	}
	newNodeSet := buildBaseNodeSet(nodePool)
	newNodeSet.SetCapi(capiNodePoolProvider)
	return applyNewNodeSet(ctx, tx, nodePool, currentNodeSet, newNodeSet, logger)
}

// buildBaseNodeSet returns a new NodeSet pre-populated with the provider-independent
// fields from the node pool. The caller must call SetNico or SetCapi before persisting.
func buildBaseNodeSet(nodePool db.NodePool) *db.NodeSet {
	return &db.NodeSet{
		ID:                   uuid.New(),
		NodePoolID:           nodePool.ID,
		KubernetesVersion:    nodePool.KubernetesVersion,
		SystemReservedMemory: nodePool.SystemReservedMemory,
		KubeReservedMemory:   nodePool.KubeReservedMemory,
		TeleportEnabled:      nodePool.TeleportEnabled,
	}
}

// applyNewNodeSet persists the new NodeSet, updates the pool's current NodeSet pointer,
// and records the config change. Shared by createNicoNodeSetIfChanged and createCAPINodeSetIfChanged.
func applyNewNodeSet(
	ctx context.Context, tx db.Tx,
	nodePool db.NodePool, prevNodeSet db.NodeSet, newNodeSet *db.NodeSet,
	logger *slog.Logger,
) (db.NodePool, db.NodeSet, error) {
	currentNodeSet, err := tx.CreateNodeSet(ctx, newNodeSet)
	if err != nil {
		return nodePool, db.NodeSet{}, fmt.Errorf("failed to create node set: %w", err)
	}
	nodePool.CurrentNodeSetID = &currentNodeSet.ID
	nodePool, err = tx.UpdateNodePool(ctx, &nodePool)
	if err != nil {
		return nodePool, currentNodeSet, fmt.Errorf("failed to update node pool: %w", err)
	}
	logger.Info("Created new current node set", "nodeSetID", currentNodeSet.ID, "desiredCount", 0)
	if err := recordNodePoolConfigChange(ctx, tx, prevNodeSet, currentNodeSet); err != nil {
		return nodePool, currentNodeSet, err
	}
	return nodePool, currentNodeSet, nil
}
