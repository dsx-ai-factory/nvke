// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodepool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nke/controller/internal/clients"
	"github.com/dsx-ai-factory/nke/controller/internal/config"
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

	cluster, err := tx.GetCluster(ctx, nodePool.ClusterID)
	if err != nil {
		return fmt.Errorf("failed to get cluster: %w", err)
	}

	var currentNodeSet db.NodeSet
	if nodePool.CurrentNodeSetID != nil {
		currentNodeSet, err = tx.GetNodeSet(ctx, *nodePool.CurrentNodeSetID)
		if err != nil {
			return fmt.Errorf("failed to get current node set: %w", err)
		}
	}

	nodePool, currentNodeSet, err = a.createNodeSetIfChanged(ctx, tx, cluster, nodePool, currentNodeSet, logger)
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
	if nodePool.ControlPlane && surgeNodes > 0 && strings.TrimSpace(cluster.ControlPlaneVIP) == "" {
		logger.Warn("Skipping control-plane surge because controlPlaneVIP is not configured")
		surgeNodes = 0
	}

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

// createNodeSetIfChanged compares the pool's provider payload and common
// fields against the current NodeSet, and creates a new NodeSet when
// anything has changed. Provider-specific payload comparison and
// construction are delegated to clients.NodeSetProvider so this stays
// provider-agnostic.
func (a *Controller) createNodeSetIfChanged(
	ctx context.Context, tx db.Tx,
	cluster db.Cluster, nodePool db.NodePool, currentNodeSet db.NodeSet,
	logger *slog.Logger,
) (db.NodePool, db.NodeSet, error) {
	if !db.NodePoolNeedsNewNodeSet(nodePool, currentNodeSet) {
		return nodePool, currentNodeSet, nil
	}

	provider, err := clients.GetProviderClient(cluster)
	if err != nil {
		return nodePool, currentNodeSet, err
	}
	newNodeSet := buildBaseNodeSet(nodePool)
	if err := provider.NodeSetProvider().ApplyNodeSetProvider(nodePool, newNodeSet); err != nil {
		return nodePool, currentNodeSet, err
	}
	return applyNewNodeSet(ctx, tx, cluster, nodePool, currentNodeSet, newNodeSet, logger)
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

// applyNewNodeSet persists the new NodeSet and updates the pool's current NodeSet pointer.
func applyNewNodeSet(
	ctx context.Context, tx db.Tx,
	cluster db.Cluster, nodePool db.NodePool, prevNodeSet db.NodeSet, newNodeSet *db.NodeSet,
	logger *slog.Logger,
) (db.NodePool, db.NodeSet, error) {
	if nodePool.ControlPlane && nodePool.CurrentNodeSetID != nil && strings.TrimSpace(cluster.ControlPlaneVIP) == "" {
		logger.Warn("Skipping control-plane NodeSet replacement because controlPlaneVIP is not configured")
		return nodePool, prevNodeSet, nil
	}

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
	return nodePool, currentNodeSet, nil
}
