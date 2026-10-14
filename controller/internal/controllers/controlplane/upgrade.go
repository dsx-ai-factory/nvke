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

	nodesNeedingRollout := cp.nodesNeedingRollout(cluster.ControlPlaneConfig.ControlPlaneConfigSpec)

	if len(nodesNeedingRollout) == 0 {
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

	if cluster.ControlPlaneConfig.MaxSurge > 0 {
		// Today surgeBudget is 0 or 1; a future MaxSurge>1 refills the full deficit
		// in one pass (parallel host provisioning) with no further upgrade-code changes.
		target := cluster.ControlPlaneConfig.DesiredCount + cp.surgeBudget()
		if deficit := target - cp.countControllerManagedNodes(); deficit > 0 {
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

// countHealthyControlPlaneNodes counts the healthy control plane nodes in a cluster.
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

	totalDesired := cp.expectedCount()

	if healthyCount < 2 || healthyCount < totalDesired {
		logger.Info("Insufficient healthy nodes - waiting for next reconcile",
			"operation", "node teardown",
			"healthyCount", healthyCount,
			"totalDesired", totalDesired)
		return false, nil
	}

	return true, nil
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
