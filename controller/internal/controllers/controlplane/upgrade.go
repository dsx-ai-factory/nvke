// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
)

func cpNodePoolMaxSurgeEnabled(pools []db.NodePool) bool {
	for _, pool := range pools {
		if pool.MaxSurge >= 1 {
			return true
		}
	}
	return false
}

func isSurgeAtClusterLevel(cluster db.Cluster, cpPools []db.NodePool) bool {
	if cpNodePoolMaxSurgeEnabled(cpPools) {
		return false
	}
	if cluster.ControlPlaneConfig.MaxSurge == 0 {
		return false
	}
	return true
}

// reconcileNodesNeedingRollout tears down control plane nodes needing rollout during rolling upgrades.
func (r *reconciler) reconcileNodesNeedingRollout(
	ctx context.Context,
	cluster db.Cluster,
	cp *controlPlane,
	allocator domainAllocator,
	newNodeProvider clients.NewNodeProvider,
) error {
	logger := r.logger.With(
		"clusterID", cluster.ID,
		"clusterName", cluster.Name,
		"kubernetesVersion", cluster.KubernetesVersion,
	)

	cpNodePools, err := r.getControlPlaneNodePools(ctx, cluster.ID)
	if err != nil {
		return err
	}

	// Get all nodes needing rollout for this cluster
	nodesNeedingRollout, err := r.getClusterNodesNeedingRollout(ctx, cluster, cp, logger)
	if err != nil {
		return err
	}

	if len(nodesNeedingRollout) == 0 {
		// Node-pool path only: reclaim any cluster-level surge node before clearing
		// recycling. The control-plane-controller path needs no separate surge
		// scaledown — the one-at-a-time rolling teardown (nodesNeedingRollout counts
		// in-flight nodes, so provisioning never outruns teardown) converges the
		// count back to DesiredCount. Cluster-level desired-count decreases are
		// handled outside this upgrade path.
		if !cp.useController {
			surgeHandled, err := r.scaleDownSurgeNodes(ctx, cluster, cp, cpNodePools, logger)
			if err != nil {
				return err
			}
			if surgeHandled {
				// if surge node deleted, return and wait for it to be deleted before next step
				return nil
			}
		}

		// If there are no nodes needing rollout and we were recycling, mark recycling completed.
		if cluster.Conditions.CPNodesRecycling.Status == db.ConditionStatusTrue {
			if err := r.updateClusterCPNodesRecyclingCondition(ctx, cluster.ID, false, "Control plane nodes recycling completed successfully", logger); err != nil {
				return err
			}
		}
		return nil
	}
	if !cluster.EnableCPNodeRecycling {
		return nil
	}

	if isSurgeAtClusterLevel(cluster, cpNodePools) {
		surgeNodes, err := r.getControlPlaneControllerManagedNodes(ctx, cluster.ID)
		if err != nil {
			return err
		}
		if cp.useController {
			// Control-plane-controller path: provision toward the surge-inclusive
			// target. All CP nodes are managed here, so the target is
			// desired + surge budget. Today surgeBudget is 0 or 1; a future
			// MaxSurge>1 refills the full deficit in one pass (parallel host
			// provisioning) with no further upgrade-code changes.
			target := cluster.ControlPlaneConfig.DesiredCount + cp.surgeBudget()
			if deficit := target - len(surgeNodes); deficit > 0 {
				if controlPlaneReplacementBlocked(cluster) {
					logger.Warn("Skipping control plane scale-up because controlPlaneVIP is not configured",
						"nodesNeedingRolloutCount", len(nodesNeedingRollout))
					return nil
				}
				created, err := r.scaleUp(ctx, cluster, deficit, "surge nodes during upgrade", allocator, newNodeProvider)
				if err != nil {
					return fmt.Errorf("failed to scale up control plane before recycling: %w", err)
				}
				if created < deficit {
					return nil
				}
			}
		} else if len(surgeNodes) == 0 {
			// Legacy node-pool mode adds a single surge node when none is present.
			if controlPlaneReplacementBlocked(cluster) {
				logger.Warn("Skipping control plane scale-up because controlPlaneVIP is not configured",
					"nodesNeedingRolloutCount", len(nodesNeedingRollout))
				return nil
			}
			if _, err := r.scaleUp(ctx, cluster, 1, "surge node during upgrade", allocator, newNodeProvider); err != nil {
				return fmt.Errorf("failed to scale up control plane before recycling: %w", err)
			}
		}
	}

	// During recycling/teardown, enforce surge-inclusive health before marking recycling.
	healthy, err := r.isControlPlaneHealthy(ctx, cluster, cp, logger)
	if err != nil {
		return err
	}
	if !healthy {
		logger.Info("Control plane not ready for node recycling - waiting for sufficient healthy nodes",
			"nodesNeedingRolloutCount", len(nodesNeedingRollout))
		return nil
	}

	if cluster.Conditions.CPNodesRecycling.Status != db.ConditionStatusTrue {
		if err := r.updateClusterCPNodesRecyclingCondition(ctx, cluster.ID, true, "Control plane nodes recycling in progress", logger); err != nil {
			return err
		}
	}

	for _, nodeNeedingRollout := range nodesNeedingRollout {
		if nodes.IsDeleteInFlight(nodeNeedingRollout) {
			logger.Info("Skipping node for deletion - node delete already in progress", "nodeID", nodeNeedingRollout.ID, "nodeName", nodeNeedingRollout.Name)
			return nil
		}
	}

	// Select the first node needing rollout for teardown
	// nodesNeedingRollout contain nodes from nodepool with maxSurge last
	// non maxSurge nodepool nodes are deleted first
	targetNode := nodesNeedingRollout[0]

	nodeLogger := logger.With(
		"nodeID", targetNode.ID,
		"nodeName", targetNode.Name,
	)

	nodeLogger.Info("Processing node for deletion in rolling upgrade")

	// Teardown old node. A delete already in flight is a skip, not a failure: the
	// next reconcile picks this up once that teardown finishes.
	started, err := r.teardownOldNode(
		ctx,
		cp,
		targetNode,
		"OS version upgrade - replacing with new version",
		"os-version-upgrade",
		nodeLogger,
	)
	if err != nil {
		return fmt.Errorf("failed to teardown old node: %w", err)
	}
	if !started {
		return nil
	}

	nodeLogger.Info("Node marked for deletion - will be replaced on next reconcile")

	return nil
}

func (r *reconciler) getControlPlaneControllerManagedNodes(ctx context.Context, clusterID uuid.UUID) ([]db.Node, error) {
	nodes, err := r.getControlPlaneNodes(ctx, clusterID)
	if err != nil {
		return nil, err
	}

	controlPlaneManagedNodes := make([]db.Node, 0)
	for _, node := range nodes {
		if !node.IsManagedByControlPlaneController() {
			continue
		}
		controlPlaneManagedNodes = append(controlPlaneManagedNodes, node)
	}
	return controlPlaneManagedNodes, nil
}

// teardownOldNode marks a control-plane node for deletion and queues its
// teardown. It reports whether it started one: a delete already in flight is an
// ordinary outcome, not a failure, so it comes back as (false, nil) rather than
// as an error the caller has to recognize by its message.
//
// The in-flight check reads the control plane the reconcile already loaded
// rather than re-querying, so every caller decides from one snapshot.
func (r *reconciler) teardownOldNode(
	ctx context.Context,
	cp *controlPlane,
	node db.Node,
	deletingMessage string,
	workflowReason string,
	logger *slog.Logger,
) (bool, error) {
	if inFlight, ok := cp.nodeWithDeleteInFlight(); ok {
		logger.Info("Skipping control-plane node teardown: another delete is in flight",
			"inFlightNodeID", inFlight.ID,
			"inFlightNodeName", inFlight.Name,
		)
		return false, nil
	}

	nodeToDelete, err := r.db.GetNodeForUpdate(ctx, node.ID)
	if err != nil {
		return false, fmt.Errorf("failed to get node: %w", err)
	}

	if err := nodes.StartTeardown(ctx, r.db, nodeToDelete, nodes.TeardownNodeInput{
		Reason: workflowReason,
	}, deletingMessage); err != nil {
		return false, err
	}

	logger.Info("Marked node for deletion", "nodeID", nodeToDelete.ID, "nodeName", nodeToDelete.Name)
	return true, nil
}

// updateClusterCPNodesRecyclingCondition updates the cluster's CP nodes recycling condition with the given status and message.
func (r *reconciler) updateClusterCPNodesRecyclingCondition(
	ctx context.Context,
	clusterID uuid.UUID,
	status bool,
	message string,
	logger *slog.Logger,
) error {
	cluster, err := r.db.GetCluster(ctx, clusterID)
	if err != nil {
		return fmt.Errorf("failed to get cluster: %w", err)
	}

	cluster.Conditions.CPNodesRecycling = db.NewCondition(status, message)
	_, err = r.db.UpdateCluster(ctx, &cluster)
	if err != nil {
		return fmt.Errorf("failed to update cluster: %w", err)
	}

	logger.Info("Updated cluster CP nodes recycling condition", "status", status, "message", message)

	return nil
}

// countHealthyControlPlaneNodes counts total healthy control plane nodes across ALL node pools in a cluster.
func (r *reconciler) countHealthyControlPlaneNodes(ctx context.Context, clusterID uuid.UUID) (int, error) {
	// Get all control plane nodes in the cluster
	nodes, err := r.getControlPlaneNodes(ctx, clusterID)
	if err != nil {
		return 0, err
	}

	healthyCount := 0
	for _, node := range nodes {
		if isHealthyControlPlaneNode(node) {
			healthyCount++
		}
	}

	return healthyCount, nil
}

func isHealthyControlPlaneNode(node db.Node) bool {
	return node.Conditions.KubernetesReady.Status == db.ConditionStatusTrue &&
		node.Conditions.Provisioned.Status == db.ConditionStatusTrue &&
		node.Conditions.Deleting.Status != db.ConditionStatusTrue &&
		node.Conditions.Pending.Status != db.ConditionStatusTrue
}

// getTotalDesiredControlPlaneNodes calculates the total DesiredCount across all control plane nodepools.
// If withSurge is true, it accounts for either nodepool-level or cluster-level maxSurge
// from control_plane_config.
func (r *reconciler) getTotalDesiredControlPlaneNodes(ctx context.Context, cluster db.Cluster, cp *controlPlane, withSurge bool) (int, error) {
	// Control-plane-controller path: desired count comes from the cluster config;
	// no control-plane nodepools are involved.
	if r.cfg.UseControlPlaneControllerForControlPlane() {
		totalDesired := cluster.ControlPlaneConfig.DesiredCount
		if withSurge && cluster.ControlPlaneConfig.MaxSurge > 0 {
			totalDesired += cp.surgeBudget()
		}
		return totalDesired, nil
	}

	// Get all control plane nodepools in the cluster
	nodePools, err := r.getControlPlaneNodePools(ctx, cluster.ID)
	if err != nil {
		return 0, err
	}

	// Sum up DesiredCount across all control plane nodepools
	totalDesired := 0
	totalMaxSurge := 0
	for _, pool := range nodePools {
		totalDesired += pool.DesiredCount
		totalMaxSurge += pool.MaxSurge
	}

	// Optionally add surge nodes (capped at 1)
	if withSurge {
		if cpNodePoolMaxSurgeEnabled(nodePools) {
			totalDesired += min(totalMaxSurge, 1)
		} else if isSurgeAtClusterLevel(cluster, nodePools) {
			totalDesired += min(cluster.ControlPlaneConfig.MaxSurge, 1)
		}
	}

	return totalDesired, nil
}

// isControlPlaneHealthy checks if the control plane has sufficient healthy nodes
// by comparing the count of healthy nodes against surge-inclusive total desired count.
// Returns true if the cluster has enough healthy nodes, false otherwise.
func (r *reconciler) isControlPlaneHealthy(
	ctx context.Context,
	cluster db.Cluster,
	cp *controlPlane,
	logger *slog.Logger,
) (bool, error) {
	healthyCount, err := r.countHealthyControlPlaneNodes(ctx, cluster.ID)
	if err != nil {
		return false, fmt.Errorf("failed to count healthy nodes: %w", err)
	}

	totalDesired, err := r.getTotalDesiredControlPlaneNodes(ctx, cluster, cp, true)
	if err != nil {
		return false, fmt.Errorf("failed to get total desired control plane nodes: %w", err)
	}

	if healthyCount < 2 || healthyCount < totalDesired {
		logger.Info("Insufficient healthy nodes - waiting for next reconcile",
			"operation", "node teardown",
			"healthyCount", healthyCount,
			"totalDesired", totalDesired)
		return false, nil
	}

	return true, nil
}

// getControlPlaneNodePools returns all control plane node pools for a cluster.
func (r *reconciler) getControlPlaneNodePools(ctx context.Context, clusterID uuid.UUID) ([]db.NodePool, error) {
	controlPlaneTrue := true
	nodePools, err := r.db.ListNodePools(ctx, db.NodePoolFilters{
		ClusterID:    &clusterID,
		ControlPlane: &controlPlaneTrue,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list control plane node pools: %w", err)
	}
	return nodePools, nil
}

// getControlPlaneNodes returns all control plane nodes for a cluster.
func (r *reconciler) getControlPlaneNodes(ctx context.Context, clusterID uuid.UUID) ([]db.Node, error) {
	controlPlaneTrue := true
	nodes, err := r.db.ListNodes(ctx, db.NodeFilters{
		ClusterID:    &clusterID,
		ControlPlane: &controlPlaneTrue,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list control plane nodes: %w", err)
	}
	return nodes, nil
}

// getClusterNodesNeedingRollout returns all control plane nodes needing rollout across all
// control plane node pools, regardless of their state (pending, deleting, etc.).
// Nodes from NodePools with maxSurge=0 are returned first, followed by nodes from NodePools
// with maxSurge>0 (surge nodes are deleted last to maintain availability during upgrades).
func (r *reconciler) getClusterNodesNeedingRollout(
	ctx context.Context,
	cluster db.Cluster,
	cp *controlPlane,
	logger *slog.Logger,
) ([]db.Node, error) {
	// Control-plane-controller path: derive rollout from each node's pinned
	// snapshot vs the cluster's current snapshot (preloaded by readControlPlane).
	if r.cfg.UseControlPlaneControllerForControlPlane() {
		return cp.nodesNeedingRollout(cluster.ControlPlaneConfig.ControlPlaneConfigSpec), nil
	}

	// Get all control plane node pools for this cluster
	cpNodePools, err := r.getControlPlaneNodePools(ctx, cluster.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to get control plane node pools: %w", err)
	}

	var surgeNodePoolDeprecatedNodes []db.Node    // Nodes from surge NodeSets to be deleted last
	var nonSurgeNodePoolDeprecatedNodes []db.Node // Nodes from non-surge NodeSets to be deleted first

	for _, nodePool := range cpNodePools {
		// Skip nodepools without a current NodeSet
		if nodePool.CurrentNodeSetID == nil {
			continue
		}

		// Get all NodeSets for this nodepool
		allNodeSets, err := r.db.ListNodeSets(ctx, db.NodeSetFilters{NodePoolID: &nodePool.ID})
		if err != nil {
			return nil, fmt.Errorf("failed to list node sets: %w", err)
		}

		// Collect nodes from deprecated NodeSets
		for _, ns := range allNodeSets {
			// Skip current NodeSet
			if ns.ID == *nodePool.CurrentNodeSetID {
				continue
			}

			// Get nodes from this deprecated NodeSet
			nodes, err := r.db.ListNodes(ctx, db.NodeFilters{NodeSetID: &ns.ID})
			if err != nil {
				return nil, fmt.Errorf("failed to list nodes: %w", err)
			}

			if len(nodes) > 0 {
				logger.Debug("Found deprecated nodes in nodepool",
					"nodePoolID", nodePool.ID,
					"nodePoolName", nodePool.Name,
					"deprecatedNodeSetID", ns.ID,
					"nodeCount", len(nodes))

				// If nodepool has maxSurge > 0, add nodes to surge list (to be deleted last)
				// Otherwise add to regular list
				if nodePool.MaxSurge > 0 {
					logger.Debug("NodePool has maxSurge - adding nodes to surge list (delete last)",
						"nodePoolID", nodePool.ID,
						"nodeSetID", ns.ID,
						"maxSurge", nodePool.MaxSurge)
					surgeNodePoolDeprecatedNodes = append(surgeNodePoolDeprecatedNodes, nodes...)
				} else {
					nonSurgeNodePoolDeprecatedNodes = append(nonSurgeNodePoolDeprecatedNodes, nodes...)
				}
			}
		}
	}

	// Add non-surge nodes first (to be deleted first), then surge nodes last
	nodesNeedingRollout := make([]db.Node, 0, len(nonSurgeNodePoolDeprecatedNodes)+len(surgeNodePoolDeprecatedNodes))
	nodesNeedingRollout = append(nodesNeedingRollout, nonSurgeNodePoolDeprecatedNodes...)
	nodesNeedingRollout = append(nodesNeedingRollout, surgeNodePoolDeprecatedNodes...)

	return nodesNeedingRollout, nil
}
