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

// reconcileNodesNeedingRollout records delete intent for control-plane nodes that
// need rollout during rolling upgrades. It reports whether it recorded a delete:
// when true the caller must end the reconcile pass, because the in-memory
// controlPlane snapshot does not yet reflect the recorded delete, so the steps
// below -- joins, rebalance (whose abandon path starts a teardown directly) and
// scale-down -- would otherwise act on a stale view and could start a second one.
func (r *reconciler) reconcileNodesNeedingRollout(
	ctx context.Context,
	cluster db.Cluster,
	cp *controlPlane,
	allocator domainAllocator,
	newNodeProvider clients.NewNodeProvider,
) (bool, error) {
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
				return false, err
			}
		}
		return false, nil
	}
	if !cluster.EnableCPNodeRecycling {
		return false, nil
	}

	if cluster.ControlPlaneConfig.MaxSurge > 0 {
		// Today surgeBudget is 0 or 1; a future MaxSurge>1 refills the full deficit
		// in one pass (parallel host provisioning) with no further upgrade-code changes.
		target := cluster.ControlPlaneConfig.DesiredCount + cp.surgeBudget()
		if deficit := target - cp.countControllerManagedNodes(); deficit > 0 {
			if r.controlPlaneReplacementBlocked(cluster) {
				logger.Warn("Skipping control plane scale-up because controlPlaneVIP is not configured",
					"nodesNeedingRolloutCount", len(nodesNeedingRollout))
				return false, nil
			}
			allocator.release(cp.failureDomains.domainOf(nodesNeedingRollout[0]))
			created, err := r.scaleUp(ctx, cluster, deficit, "surge nodes during upgrade", allocator, newNodeProvider)
			if err != nil {
				return false, fmt.Errorf("failed to scale up control plane before recycling: %w", err)
			}
			if created < deficit {
				return false, nil
			}
		}
	}

	// During recycling/teardown, enforce surge-inclusive health before marking recycling.
	healthy, err := r.isControlPlaneHealthy(ctx, cluster, cp, logger)
	if err != nil {
		return false, err
	}
	if !healthy {
		logger.Info("Control plane not ready for node recycling - waiting for sufficient healthy nodes",
			"nodesNeedingRolloutCount", len(nodesNeedingRollout))
		return false, nil
	}

	if cluster.Conditions.CPNodesRecycling.Status != db.ConditionStatusTrue {
		if err := r.updateClusterCPNodesRecyclingCondition(ctx, cluster.ID, true, "Control plane nodes recycling in progress", logger); err != nil {
			return false, err
		}
	}

	// A delete already in flight anywhere on the control plane -- recorded and
	// waiting behind the serialized delete loop, or already tearing down -- means
	// this pass has nothing to do. reconcileRequestedDeletes drains one teardown at
	// a time, and the next reconcile picks the rollout back up once it clears. This
	// also keeps the step idempotent: the node stays in nodesNeedingRollout until
	// it is gone, so once the intent below is recorded, this check skips it rather
	// than recording it twice.
	if inFlight, ok := cp.nodeWithDeleteInFlight(); ok {
		logger.Info("Skipping rollout teardown: a control-plane node delete is already in flight",
			"inFlightNodeID", inFlight.ID,
			"inFlightNodeName", inFlight.Name,
		)
		return false, nil
	}

	targetNode := nodesNeedingRollout[0]

	nodeLogger := logger.With(
		"nodeID", targetNode.ID,
		"nodeName", targetNode.Name,
	)

	// The surge-inclusive health check above proves the replacement is Ready, not
	// that it has Joined etcd, and this path skips the live-delete guard. Without a
	// joined-survivor check, a rollout delete could be promoted while the
	// replacement is still only Ready and then remove the last joined member,
	// breaking quorum with the teardown's etcd member removal skipped. Require the
	// surviving control-plane members (excluding the target and pending nodes) to
	// keep etcd quorum before recording intent. A survivor counts only when it is
	// both Joined and KubernetesReady: a joined member whose etcd is down must not
	// prop up the quorum count. Mirrors canStartRemediation's quorum precondition.
	totalMembers, joinedMembers := 0, 0
	for _, node := range cp.nodes {
		if node.ID == targetNode.ID || isPending(node) {
			continue
		}
		totalMembers++
		if node.Conditions.Joined.Status == db.ConditionStatusTrue && isKubernetesReady(node) {
			joinedMembers++
		}
	}
	if quorum := (totalMembers / 2) + 1; joinedMembers < quorum {
		nodeLogger.Info("Skipping rollout teardown: joined control-plane survivors would not keep etcd quorum",
			"joinedMembers", joinedMembers,
			"totalMembers", totalMembers,
			"quorum", quorum,
		)
		return false, nil
	}

	nodeLogger.Info("Recording rollout delete for control-plane node")

	// Record the delete as intent and let reconcileRequestedDeletes start the
	// teardown -- the same path as a user delete, remediation and scale-down, so
	// every control-plane delete serializes in that one loop. Controllers enforce
	// their own preconditions: the surge-inclusive health check above is the
	// rollout's, so skip the live-API guard and the quorum floor (rollout replaces
	// the node rather than shrinking the control plane).
	if err := nodes.DeleteWithTx(ctx, r.db, targetNode.ID,
		nodes.WithReason("os-version-upgrade"),
		nodes.WithAllowCPNode(true),
		nodes.WithSkipLiveDeleteGuard(),
		nodes.WithSkipQuorumFloor(),
	); err != nil {
		return false, fmt.Errorf("recording rollout delete for node %s (%s): %w", targetNode.ID, targetNode.Name, err)
	}

	nodeLogger.Info("Recorded rollout delete; the serialized delete loop will start the teardown")

	return true, nil
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
	// A node with an accepted delete -- recorded and waiting behind the serialized
	// delete loop, or already tearing down -- is on its way out, so it does not
	// count toward the healthy nodes the rollout needs before it tears the next one
	// down. Excluding only Deleting here would let a node the rollout just queued
	// still count as healthy until its teardown started.
	return node.Conditions.KubernetesReady.Status == db.ConditionStatusTrue &&
		node.Conditions.Provisioned.Status == db.ConditionStatusTrue &&
		!nodes.IsDeleteInFlight(node) &&
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
