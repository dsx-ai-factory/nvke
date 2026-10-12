// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeset

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"

	"github.com/google/uuid"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/filters"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/naming"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
)

// Controller is responsible for reconciling node sets.
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

func (c *Controller) SetupWithManager(mgr manager.Manager) error {
	return dbreconcile.SetupController(
		mgr,
		"node-set-controller",
		c,
		dbwrapper.NewNodeSetSource(c.db, dbfilters.NodeSet{}),
		dbwrapper.NewNodeSource(c.db, dbfilters.Node{}, nodeSetKeysForNode),
	)
}

func nodeSetKeysForNode(_ context.Context, node db.Node) ([]dbreconcile.Key, error) {
	if node.NodeSetID == uuid.Nil {
		return nil, nil
	}
	return []dbreconcile.Key{dbreconcile.NodeSetKey(node.NodeSetID)}, nil
}

// Reconcile NodeSets, for each NodeSet:
// - Examine the NodeSet.DesiredCount
//   - Create pending records for any missing nodes.
//   - The node pool node scheduler will try to provision Pending Nodes when resources are available.
//   - Delete pending nodes when the count is above the desired count.
//   - !!! NOTE: Only pending nodes will be deleted. We do not currently delete or mark for deletion active nodes.
//   - For non-current NodeSets the NodePoolController will have set the DesiredCount to 0.
//   - So we will effectively delete all pending nodes from any non-current NodeSets.
//
// - (Future) Auto-deprovision down to desired count by applying the nke.nvidia.com/maintenance-required label.
// - (Future) Perform in-place upgrades of existing node sets.
//   - Only non-current node sets may be upgraded.
func (c *Controller) Reconcile(ctx context.Context, key dbreconcile.Key) (reconcile.Result, error) {
	err := c.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		nodeSet, err := tx.GetNodeSet(ctx, key.ID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				c.logger.Debug("Node set not found, skipping node set reconciliation", "nodeSetID", key.ID)
				return nil
			}
			return fmt.Errorf("failed to get node set %s: %w", key.ID, err)
		}

		return c.reconcileNodeSet(ctx, tx, nodeSet)
	})
	return reconcile.Result{}, err
}

func (c *Controller) reconcileNodeSet(ctx context.Context, tx db.Tx, nodeSet db.NodeSet) error {
	nodePool, err := tx.GetNodePool(ctx, nodeSet.NodePoolID)
	if err != nil {
		return fmt.Errorf("failed to get node pool: %w", err)
	}

	nsLogger := c.logger.With(
		"nodeSetID", nodeSet.ID,
		"nodePoolID", nodePool.ID,
	)

	// When the control-plane controller manages control-plane provisioning it
	// fully owns the control-plane node lifecycle (create and delete), tracking
	// them as cluster-level no-nodepool nodes. Skip control-plane node sets
	// entirely so the two controllers never both act on them; any legacy
	// nodepool/node-set control-plane nodes are migrated to the control-plane
	// controller.
	if nodePool.ControlPlane && c.cfg.UseControlPlaneControllerForControlPlane() {
		nsLogger.Debug("Control-plane node set fully managed by control-plane controller; skipping")
		return nil
	}

	if nodePool.AutoProvisionDisabled {
		nsLogger.Debug("Node pool has auto-provision disabled, skipping node set reconciliation")
		return nil
	}

	// If the node pool is deleting, skip reconciliation.
	if nodePool.Conditions.Deleting.Status == db.ConditionStatusTrue {
		nsLogger.Debug("Node pool is deleting, skipping node set reconciliation")
		return nil
	}

	// Calculate how many nodes to provision
	nsLogger, deficit, err := c.calculateNumberToProvision(ctx, tx, nodeSet, nsLogger)
	if err != nil {
		return fmt.Errorf("failed to calculate number to provision: %w", err)
	}

	switch {
	case deficit == 0:
		nsLogger.Debug("Node set is at desired count")

	case deficit > 0:
		nsLogger.Debug("Node set is below desired count")

		cluster, err := tx.GetCluster(ctx, nodePool.ClusterID)
		if err != nil {
			return fmt.Errorf("failed to get cluster: %w", err)
		}

		// Create a pending node record for each node we need to provision, to come up to the desired count.
		// The node pool node scheduler will then try to provision the pending node records when resources are available.
		for range deficit {
			createdNode, err := c.createPendingNode(ctx, tx, cluster, nodePool, nodeSet)
			if err != nil {
				return fmt.Errorf("failed to create node: %w", err)
			}
			nsLogger.Info("Created pending node", "nodeID", createdNode.ID, "nodeName", createdNode.Name)
		}

	case deficit < 0:
		nsLogger.Debug("Node set is above desired count, delete pending nodes, skip auto-teardown")
		// We could implement auto-teardown to get back down to the desired count.
		// This is potentially risky, because a misconfiguration or a bug could quickly cause an SLO breach or outage.
		// Additionally if we implement the constant maintenance via label it will take care of itself.
		// - Whenever a node is terminated for maintenance if the node pool count is >= desired count, no new node will be auto-provisioned back in.
		// - so it would eventually get back to the desired count.
		// - We still have the delete node capability from the API.
		//
		// For these reasons, we only delete pending node records if there is a surplus.
		// So that the node pool node scheduler does not provision nodes into a node pool, and thus consume forge resources, that would be above the desired count.

		pendingNodes, err := tx.ListNodes(ctx, db.NodeFilters{
			NodeSetID:           &nodeSet.ID,
			Pending:             ptr.To(true),
			ProvisioningClaimed: ptr.To(false),
		})
		if err != nil {
			return fmt.Errorf("failed to list nodes: %w", err)
		}

		// Try to delete nodes, until we have deleted the surplus, or we have deleted all pending nodes.
		// Skipping any that are locked (being provisioned), we will either get another pending node to delete,
		// or the next Reconcile() will try again.
		// To prevent race conditions where this controller attempts to delete a pending
		// node that has a provision workflow running, but hasn't marked the node as provisioning yet.
		surplus := max(deficit*-1, 0)
		deletedCount := 0
		for _, pendingNode := range pendingNodes {
			if deletedCount >= surplus {
				break
			}

			deleted, err := c.deletePendingNode(ctx, tx, pendingNode, nsLogger)
			if err != nil {
				return fmt.Errorf("failed to delete pending node: %w", err)
			}
			if !deleted {
				continue
			}

			deletedCount++
			nsLogger.Info("Deleted pending node", "nodeID", pendingNode.ID, "nodeName", pendingNode.Name, "deletedCount", deletedCount, "surplus", surplus)
		}
	}

	return nil
}

func (c *Controller) calculateNumberToProvision(ctx context.Context, tx db.Tx, nodeSet db.NodeSet, nsLogger *slog.Logger) (*slog.Logger, int, error) {
	nodeSetNodes, err := tx.ListNodes(ctx, db.NodeFilters{NodeSetID: &nodeSet.ID})
	if err != nil {
		return nsLogger, 0, fmt.Errorf("failed to list nodes: %w", err)
	}

	// Note: The actual count includes nodes that are provisioning / deleting down.
	actualCount := len(nodeSetNodes)

	// Calculate how many nodes we need to reach desired state
	// example: desiredCount = 10, actual = 4, deficit = 6
	// example: desiredCount = 10, actual = 14, deficit = -4
	deficit := nodeSet.DesiredCount - actualCount

	nsLogger = nsLogger.With(
		"desiredCount", nodeSet.DesiredCount,
		"actualCount", actualCount,
		"deficit", deficit,
	)

	return nsLogger, deficit, nil
}

func (c *Controller) createPendingNode(ctx context.Context, tx db.Tx, cluster db.Cluster, nodePool db.NodePool, nodeSet db.NodeSet) (db.Node, error) {
	if !nodePool.IsCurrentNodeSet(nodeSet.ID) {
		return db.Node{}, fmt.Errorf("node set is non-current")
	}

	existingNodeNames, err := tx.ListClusterNodeNames(ctx, cluster.ID)
	if err != nil {
		return db.Node{}, fmt.Errorf("failed to list cluster node names: %w", err)
	}

	dbNode := db.Node{
		ClusterID:    cluster.ID,
		NodePoolID:   nodePool.ID,
		NodeSetID:    nodeSet.ID,
		ControlPlane: nodePool.ControlPlane,
		Name:         naming.GenerateNodeName(existingNodeNames, cluster.Name, nodePool.NodeNamePrefix, rand.IntN),
		Conditions: db.NodeConditions{
			Pending:                 db.NewCondition(true, "Pending node provisioning"),
			ProvisioningClaimed:     db.NewCondition(false, "Provisioning workflow has not claimed node"),
			Provisioning:            db.NewCondition(false, "Not yet provisioning"),
			ReadyToJoin:             db.NewCondition(false, "Not yet ready to join"),
			Joined:                  db.NewCondition(false, "Not yet joined"),
			Provisioned:             db.NewCondition(false, "Not yet provisioned"),
			Deleting:                db.NewCondition(false, "Node is not being deleted"),
			KubernetesReady:         db.NewCondition(false, "Status unknown"),
			KubernetesUnschedulable: db.NewCondition(false, "Status unknown"),
			NodeAgentReady:          db.NewCondition(false, "Status unknown"),
		},
	}
	nodes.PinAgentProfile(&dbNode, c.cfg)
	nodeProvider, err := clients.GetNewNodeProvider(cluster)
	if err != nil {
		return db.Node{}, err
	}
	if err := nodeProvider.ApplyNodePoolNodePayload(nodePool, &dbNode); err != nil {
		return db.Node{}, err
	}

	createdNode, err := tx.CreateNode(ctx, &dbNode)
	if err != nil {
		return db.Node{}, fmt.Errorf("failed to create node: %w", err)
	}

	return createdNode, nil
}

func (c *Controller) deletePendingNode(ctx context.Context, tx db.Tx, pendingNode db.Node, nsLogger *slog.Logger) (bool, error) {
	// Try to acquire lock - fail/continue fast if already held
	acquired, err := tx.TryAcquireNodeProvisioningLock(ctx, pendingNode.ID)
	if err != nil {
		return false, fmt.Errorf("failed to try to acquire lock for pending node %s: %w", pendingNode.ID, err)
	}
	if !acquired {
		nsLogger.Debug("Pending Node is being provisioned, skipping deletion", "nodeID", pendingNode.ID)
		return false, nil
	}

	nodeForUpdate, err := tx.GetNodeForUpdate(ctx, pendingNode.ID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			nsLogger.Debug("Pending node disappeared before deletion", "nodeID", pendingNode.ID)
			return false, nil
		}
		return false, fmt.Errorf("failed to get node for update: %w", err)
	}
	if nodeForUpdate.Conditions.Pending.Status != db.ConditionStatusTrue ||
		nodeForUpdate.Conditions.ProvisioningClaimed.Status == db.ConditionStatusTrue {
		nsLogger.Debug("Pending node is no longer eligible for deletion",
			"nodeID", pendingNode.ID,
			"pending", nodeForUpdate.Conditions.Pending.Status,
			"provisioningClaimed", nodeForUpdate.Conditions.ProvisioningClaimed.Status,
		)
		return false, nil
	}

	err = tx.DeleteNode(ctx, pendingNode.ID)
	if err != nil {
		return false, fmt.Errorf("failed to delete node: %w", err)
	}

	return true, nil
}
