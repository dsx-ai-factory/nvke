// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/nodes"
)

// remediateMachines processes control plane node remediation for the given cluster.
// It checks for nodes marked for recycling and safely deletes them one at a time,
// waiting for replacements to become healthy before proceeding.
//
//nolint:nonamedreturns // named returns used for clarity because we are returning a boolean for a non "question" function.
func (r *reconciler) remediateMachines(ctx context.Context, cluster db.Cluster, controlPlane *controlPlane) (requeue bool, err error) {
	log := r.logger.With(
		"clusterID", cluster.ID,
		"clusterName", cluster.Name,
		"cpNodeCount", controlPlane.size(),
	)

	// Find all nodes marked for remediation
	nodesForRemediation := controlPlane.nodesForRemediation()
	if len(nodesForRemediation) == 0 {
		log.Debug("No control plane nodes marked for remediation")
		return false, nil
	}

	log.Debug("Found control plane nodes marked for remediation", "nodeCount", len(nodesForRemediation))

	// Select the node to remediate based on prioritization rules
	nodeToRemediate := selectNodeToRemediate(nodesForRemediation)

	log = log.With(
		"nodeToRemoveID", nodeToRemediate.ID,
		"nodeToRemoveName", nodeToRemediate.Name,
	)

	// Check pre-conditions for remediation
	if !canStartRemediation(log, controlPlane, nodeToRemediate) {
		return false, nil
	}

	log.Info("Starting control plane node deletion as part of remediation")

	// Trigger deletion of the selected node
	if err := nodes.DeleteWithTx(ctx, r.db, nodeToRemediate.ID,
		nodes.WithReason("Control plane node remediation"),
		nodes.WithAllowCPNode(true),
	); err != nil {
		return false, fmt.Errorf("deleting node %s (%s) marked for remediation: %w", nodeToRemediate.ID, nodeToRemediate.Name, err)
	}

	log.Info("Control plane node marked for deletion, teardown workflow queued")
	return true, nil
}

// selectNodeToRemediate selects the next node to remediate based on prioritization rules:
// 1. Oldest marked node that is still pending
// 2. Oldest marked node that is not yet provisioned
// 3. Oldest marked node that is not yet KubernetesReady
// 4. Oldest marked node that is not yet NodeAgentReady
// last: Otherwise, oldest marked node
func selectNodeToRemediate(markedNodes []db.Node) db.Node {
	// Sort by creation time (oldest first)
	sort.Slice(markedNodes, func(i, j int) bool {
		return markedNodes[i].CreatedAt.Before(markedNodes[j].CreatedAt)
	})

	// First, look for the oldest pending (not yet provisioned) node
	for _, node := range markedNodes {
		if isPending(node) {
			return node
		}
	}

	// Second, look for the oldest node that is not yet provisioned
	for _, node := range markedNodes {
		if !isProvisioned(node) {
			return node
		}
	}

	// Third, look for the oldest node that is not yet KubernetesReady
	for _, node := range markedNodes {
		if !isKubernetesReady(node) {
			return node
		}
	}

	// Fourth, look for the oldest node that is not yet NodeAgentReady
	for _, node := range markedNodes {
		if !isNodeAgentReady(node) {
			return node
		}
	}

	// Otherwise, return the oldest marked node
	return markedNodes[0]
}

// canStartRemediation checks all pre-conditions required before starting remediation on a node.
// Returns true if remediation can proceed, false otherwise.
func canStartRemediation(log *slog.Logger, controlPlane *controlPlane, nodeToRemove db.Node) bool {
	// Pre-condition 1: Cluster must have at least 2 control plane machines
	if controlPlane.size() < 2 {
		log.Info("Cluster has fewer than 2 control plane nodes, skipping remediation")
		return false
	}

	// Pre-condition 2: Sequential Execution - replacement for previous remediation must be created.
	// Check that current size matches expected count before deleting another node.
	expectedCount := controlPlane.expectedCount()
	if controlPlane.size() < expectedCount {
		log.Info("Control plane has fewer nodes than expected, waiting for scale-up before continuing remediation",
			"currentSize", controlPlane.size(),
			"expectedCount", expectedCount,
		)
		return false
	}

	// Pre-condition 3: Stable State Provisioning - there are no healthy machines still provisioning
	if controlPlane.hasHealthyNodesProvisioning() {
		log.Info("Control plane has nodes provisioning, skipping remediation")
		return false
	}

	// Pre-condition 4: Stable State Deletion - no nodes currently marked for deletion
	if controlPlane.hasNodesMarkedForDeletion() {
		log.Info("Control plane has nodes marked for deletion, skipping remediation")
		return false
	}

	// Pre-condition 5: Enough healthy nodes to bootstrap another node.
	// Healthy in this case means the kubernetes components are working, it contains all the postgres data
	// and the node agent is ready.
	// We need at least one extra node (on top of the node to remove) that has all the postgres data
	// (so it can be replicated) and a healthy node agent so we can extract a join token for the new node.
	additionalHealthyNodes := 0
	for _, node := range controlPlane.nodes {
		if node.ID == nodeToRemove.ID {
			continue
		}
		if isKubernetesReady(node) && isProvisioned(node) && isNodeAgentReady(node) {
			log.Info("Found additional healthy node that could support bootstrapping", "healthyNodeName", node.Name)
			additionalHealthyNodes++
		}
	}
	if additionalHealthyNodes < 1 {
		log.Info("Not enough provisioned and healthy nodes to bootstrap another node, skipping remediation",
			"additionalHealthyNodes", additionalHealthyNodes,
		)
		return false
	}

	// Pre-condition 6: Preserving Etcd quorum (using KubernetesReady as proxy):
	// The runbook that removes the etcd member will check this for real against the etcd API and abort the deletion if the
	// quorum could be lost.
	targetTotalMembers, targetUnhealthyMembers := 0, 0
	for _, node := range controlPlane.nodes {
		if node.ID == nodeToRemove.ID {
			// We skip the node to remove since it won't be part of the etcd cluster.
			continue
		}

		// We consider pending nodes not part of the etcd cluster yet.
		if isPending(node) {
			continue
		}

		targetTotalMembers++

		if !isKubernetesReady(node) {
			// We consider nodes that are not KubernetesReady as unhealthy members.
			targetUnhealthyMembers++
		}
	}

	targetQuorum := (targetTotalMembers / 2) + 1
	canSafelyRemoveNode := targetTotalMembers-targetUnhealthyMembers >= targetQuorum
	if !canSafelyRemoveNode {
		log.Info("Control plane would lose etcd quorum if we removed node, skipping remediation",
			"targetTotalMembers", targetTotalMembers, "targetUnhealthyMembers", targetUnhealthyMembers, "targetQuorum", targetQuorum,
		)
		return false
	}

	log.Info("Control plane can safely remove node, remediation can proceed",
		"expectedCPCount", expectedCount,
		"additionalHealthyNodes", additionalHealthyNodes,
		"targetTotalMembers", targetTotalMembers, "targetUnhealthyMembers", targetUnhealthyMembers, "targetQuorum", targetQuorum)

	return true
}
