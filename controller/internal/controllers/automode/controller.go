// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package automode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/filters"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/workflows"
)

// Controller is responsible for auto-mode operations.
type Controller struct {
	cfg    config.Config
	db     *dbwrapper.DB
	logger *slog.Logger
}

// NewController creates a new auto-mode controller.
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

// SetupWithManager wires the controller into controller-runtime with DB-backed sources.
func (c *Controller) SetupWithManager(mgr manager.Manager) error {
	return dbreconcile.SetupController(
		mgr,
		"auto-mode-controller",
		c,
		c.db.NodePoolSource(dbfilters.NodePool{ControlPlane: ptr.To(false)}),
		c.db.NodeSetSource(dbfilters.NodeSet{}, nodePoolKeysForNodeSet),
		c.db.NodeSource(dbfilters.Node{ControlPlane: ptr.To(false)}, nodePoolKeysForNode),
	)
}

func nodePoolKeysForNodeSet(nodeSet db.NodeSet) []dbreconcile.Key {
	if nodeSet.NodePoolID == uuid.Nil {
		return nil
	}
	return []dbreconcile.Key{dbreconcile.NodePoolKey(nodeSet.NodePoolID)}
}

func nodePoolKeysForNode(node db.Node) []dbreconcile.Key {
	if node.NodePoolID == uuid.Nil {
		return nil
	}
	return []dbreconcile.Key{dbreconcile.NodePoolKey(node.NodePoolID)}
}

// Reconcile applies auto-mode for a single NodePool key.
func (c *Controller) Reconcile(ctx context.Context, key dbreconcile.Key) (reconcile.Result, error) {
	err := c.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		nodePool, err := tx.GetNodePoolForUpdate(ctx, key.ID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				c.logger.Debug("Node pool not found, skipping auto-mode reconciliation", "nodePoolID", key.ID)
				return nil
			}
			return fmt.Errorf("failed to get node pool: %w", err)
		}

		return c.reconcileAutoMode(ctx, tx, nodePool)
	})
	return reconcile.Result{}, err
}

// reconcileAutoMode:
//
// - Selects the oldest nodes from deprecated NodeSets that are eligible for recycling.
// - Applies the NeedsRecycle node condition, respecting the recycleBudget.
//   - Only applied if (desiredCount - availableCount) < recycleBudget.
//
// NVSentinel (the downstream consumer of NeedsRecycle conditions) may:
// - Allow less recycle parallelism than recycleBudget at the NKE layer.
// - Consider additional factors, such as cordon state of other nodes.
// - Enforce maintenance windows, leaving NeedsRecycle nodes in place until customer-specified windows.
func (c *Controller) reconcileAutoMode(ctx context.Context, tx db.Tx, nodePool db.NodePool) error {
	logger := c.logger.With(
		"nodePoolID", nodePool.ID,
		"nodePoolName", nodePool.Name,
		"nodePoolRecycleBudget", nodePool.RecycleBudget,
	)

	if nodePool.AutoModeDisabled {
		logger.Debug("Node pool has auto-mode disabled, skipping auto-mode reconciliation")
		return nil
	}

	if nodePool.RecycleBudget <= 0 {
		logger.Debug("Node pool has no recycle budget, skipping auto-mode reconciliation")
		return nil
	}

	if nodePool.ControlPlane {
		logger.Debug("Node pool is a control plane, skipping auto-mode reconciliation")
		return nil
	}

	if nodePool.Conditions.Deleting.Status == db.ConditionStatusTrue {
		logger.Debug("Node pool is deleting, skipping auto-mode reconciliation")
		return nil
	}

	if nodePool.CurrentNodeSetID == nil || *nodePool.CurrentNodeSetID == uuid.Nil {
		logger.Debug("Node pool has no current node set, skipping auto-mode reconciliation")
		return nil
	}

	nodes, err := c.selectNodesForRecycling(ctx, tx, nodePool)
	if err != nil {
		return fmt.Errorf("failed to select nodes for recycling: %w", err)
	}

	for _, node := range nodes {
		logger.Info("Marking node for auto-mode recycling", "nodeID", node.ID, "nodeName", node.Name)

		// Re-read under a row lock so MarkedForRecycling is applied on top of
		// the latest persisted state.
		lockedNode, err := tx.GetNodeForUpdate(ctx, node.ID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				// Node was deleted between selection and locking; skip it.
				continue
			}
			return fmt.Errorf("failed to lock node: %w", err)
		}

		lockedNode.Conditions.MarkedForRecycling = db.NewCondition(true, "Marked for auto-mode recycling")
		if _, err := tx.UpdateNode(ctx, &lockedNode); err != nil {
			return fmt.Errorf("failed to update node conditions: %s: %w", lockedNode.ID, err)
		}

		workflowID := constants.ApplyK8sNodeConditionWorkflowPrefix + lockedNode.ID.String()
		_, err = tx.CreateWorkflowOutboxEntry(ctx, workflows.ApplyK8sNodeConditionWorkflowName, workflowID, workflows.ApplyK8sNodeConditionInput{
			NodeID: lockedNode.ID,
			NodeCondition: corev1.NodeCondition{
				Type:               constants.NodeConditionTypeNeedsRecycle,
				Status:             corev1.ConditionTrue,
				Reason:             "Deprecated",
				Message:            "NKE auto-mode recycling deprecated node",
				LastTransitionTime: metav1.Now(),
			},
		})
		if err != nil {
			return fmt.Errorf("failed to create workflow outbox entry: %s: %w", lockedNode.ID, err)
		}
	}

	return nil
}

func (c *Controller) selectNodesForRecycling(ctx context.Context, tx db.Tx, nodePool db.NodePool) ([]db.Node, error) {
	// Available nodes are nodes that are:
	// 1. K8s ready
	// 2. Not Cordoned (unschedulable)
	// 3. Not Deleting
	// 4. Not Marked for Recycling
	// 5. Not Marked for Repair
	//
	// This is a relatively conservative selection of nodes for recycling.
	// The intention being to err on the side of recycling too few nodes rather than too many at any given time.
	availableNodes, err := tx.ListNodes(ctx, db.NodeFilters{
		NodePoolID:              &nodePool.ID,
		KubernetesReady:         ptr.To(true),
		KubernetesUnschedulable: ptr.To(false),
		Deleting:                ptr.To(false),
		MarkedForRecycling:      ptr.To(false),
		MarkedForRepair:         ptr.To(false),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list available nodes: %w", err)
	}

	//
	// Example 1:
	//  nodePool.DesiredCount = 250
	//  availableCount = 248
	//  nodePool.RecycleBudget = 10
	//  availableDeficit = 250 - 248 = 2
	//  iterationRecycleCount = 10 - 2 = 8 -> 8 nodes to recycle
	//
	// Example 2 (insufficient available capacity):
	//  nodePool.DesiredCount = 250
	//  availableCount = 230 (8 nodes not K8s ready, 2 nodes cordoned, 2 nodes deleting, 8 nodes marked for recycling)
	//  nodePool.RecycleBudget = 10
	//  availableDeficit = 250 - 230 = 20
	//  iterationRecycleCount = 10 - 20 = -10 -> 0 nodes to recycle
	//
	// Example 3 (surplus available capacity):
	//  nodePool.DesiredCount = 250
	//  availableCount = 260 (10 extra available nodes beyond desired)
	//  nodePool.RecycleBudget = 10
	//  availableDeficit = 250 - 260 = -10 -> 0
	//  iterationRecycleCount = 10 - 0 = 10 -> 10 nodes to recycle
	//

	// A negative availableDeficit means we have more available nodes than desired (node pool desired value reduced from a prior value).
	// We can just set it to 0, which will allow the full recycle budget to be used, effectively treating the recycle budget as a hard cap.
	availableDeficit := max(nodePool.DesiredCount-len(availableNodes), 0)

	iterationRecycleCount := max(nodePool.RecycleBudget-availableDeficit, 0)

	nodes, err := tx.ListNodesEligibleForRecycling(ctx, nodePool.ID, iterationRecycleCount)
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	return nodes, nil
}
