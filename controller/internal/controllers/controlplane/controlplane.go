// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/internal/db"
)

// controlPlane represents a control plane in the database.
// It is a read-only view of the control plane nodes and node pools.
type controlPlane struct {
	nodes     []db.Node
	nodePools []nodePool
	nodeSets  []db.NodeSet
}

// readControlPlane reads the control plane from the database.
func readControlPlane(ctx context.Context, database db.Queries, clusterID uuid.UUID) (*controlPlane, error) {
	cpTrue := true
	nodes, err := database.ListNodes(ctx, db.NodeFilters{
		ClusterID:    &clusterID,
		ControlPlane: &cpTrue,
	})
	if err != nil {
		return nil, fmt.Errorf("listing control plane nodes: %w", err)
	}

	dbNodePools, err := database.ListNodePools(ctx, db.NodePoolFilters{
		ClusterID:    &clusterID,
		ControlPlane: &cpTrue,
	})
	if err != nil {
		return nil, fmt.Errorf("listing control plane node pools: %w", err)
	}

	// Build node pools with their associated node sets and nodes
	nodePools := make([]nodePool, 0, len(dbNodePools))
	allNodeSets := []db.NodeSet{}

	for _, np := range dbNodePools {
		dbNodeSets, err := database.ListNodeSets(ctx, db.NodeSetFilters{NodePoolID: &np.ID})
		if err != nil {
			return nil, fmt.Errorf("listing node sets for node pool %s: %w", np.ID, err)
		}

		// Add to flat list
		allNodeSets = append(allNodeSets, dbNodeSets...)

		// Build node sets with their associated nodes
		nodeSets := make([]nodeSet, 0, len(dbNodeSets))
		for _, ns := range dbNodeSets {
			var nsNodes []db.Node
			for _, node := range nodes {
				if node.NodeSetID == ns.ID {
					nsNodes = append(nsNodes, node)
				}
			}
			nodeSets = append(nodeSets, nodeSet{NodeSet: ns, nodes: nsNodes})
		}

		nodePools = append(nodePools, nodePool{NodePool: np, nodeSets: nodeSets})
	}

	return &controlPlane{nodes: nodes, nodePools: nodePools, nodeSets: allNodeSets}, nil
}

// hasNodesMarkedForDeletion returns true if there is at least one node that is marked for deletion.
func (c *controlPlane) hasNodesMarkedForDeletion() bool {
	return slices.ContainsFunc(c.nodes, isMarkedForDeletion)
}

// hasNodesProvisioning returns true if there is at least one node that is not marked for
// remediation that is not kubernetes ready or not provisioned yet.
func (c *controlPlane) hasHealthyNodesProvisioning() bool {
	for _, node := range c.nodes {
		if !isMarkedForRecycling(node) &&
			!isMarkedForDeletion(node) &&
			(!isKubernetesReady(node) || !isProvisioned(node)) {
			return true
		}
	}
	return false
}

// size returns the number of nodes in the control plane.
func (c *controlPlane) size() int {
	return len(c.nodes)
}

// nodesForRemediation returns the nodes that are marked for remediation.
func (c *controlPlane) nodesForRemediation() []db.Node {
	var nodes []db.Node
	for _, node := range c.nodes {
		if isMarkedForRecycling(node) {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

// nodeSet combines a node set with its associated nodes.
type nodeSet struct {
	db.NodeSet
	nodes []db.Node
}

// nodePool combines a node pool with its associated node sets.
type nodePool struct {
	db.NodePool
	nodeSets []nodeSet
}

// expectedCount returns the expected number of control plane nodes.
func (c *controlPlane) expectedCount() int {
	total := 0
	for _, np := range c.nodePools {
		total += np.expectedCount()
	}
	return total
}

// countDesiredNodes returns the total desired number of control plane nodes (without surge).
func (c *controlPlane) countDesiredNodes() int {
	total := 0
	for _, np := range c.nodePools {
		total += np.DesiredCount
	}
	return total
}

// countHealthyNodes returns the number of healthy control plane nodes.
// A node is considered healthy if it is KubernetesReady, Provisioned,
// not Deleting, and not Pending.
func (c *controlPlane) countHealthyNodes() int {
	count := 0
	for _, node := range c.nodes {
		if node.Conditions.KubernetesReady.Status == db.ConditionStatusTrue &&
			node.Conditions.Provisioned.Status == db.ConditionStatusTrue &&
			node.Conditions.Deleting.Status != db.ConditionStatusTrue &&
			node.Conditions.Pending.Status != db.ConditionStatusTrue {
			count++
		}
	}
	return count
}

// countDeprecatedNodes returns the total count of deprecated nodes
// across all control plane node pools.
func (c *controlPlane) countDeprecatedNodes() int {
	total := 0
	for _, np := range c.nodePools {
		total += np.countDeprecatedNodes()
	}
	return total
}

// expectedCount returns the expected number of nodes for this node pool.
// This accounts for surge nodes during upgrades:
// - Normal operation (no deprecated nodes): expectedCount = DesiredCount
// - During upgrade (has deprecated nodes): expectedCount = DesiredCount + min(MaxSurge, deprecatedCount)
func (np *nodePool) expectedCount() int {
	return np.DesiredCount + min(np.MaxSurge, np.countDeprecatedNodes())
}

// countDeprecatedNodes returns the number of nodes in deprecated node sets.
// This indicates an upgrade/migration is in progress where surge nodes may be expected.
func (np *nodePool) countDeprecatedNodes() int {
	count := 0
	for _, ns := range np.nodeSets {
		if !np.IsCurrentNodeSet(ns.ID) {
			count += len(ns.nodes)
		}
	}
	return count
}

// isKubernetesReady returns true if the node has a KubernetesReady condition that is true.
func isKubernetesReady(node db.Node) bool {
	return node.Conditions.KubernetesReady.Status == db.ConditionStatusTrue
}

// isPending returns true if the node has a Pending condition that is true.
func isPending(node db.Node) bool {
	return node.Conditions.Pending.Status == db.ConditionStatusTrue
}

// isMarkedForRecycling returns true if the node has a MarkedForRecycling condition that is true.
func isMarkedForRecycling(node db.Node) bool {
	return node.Conditions.MarkedForRecycling.Status == db.ConditionStatusTrue
}

// isMarkedForDeletion returns true if the node has a Deleting condition that is true.
func isMarkedForDeletion(node db.Node) bool {
	return node.Conditions.Deleting.Status == db.ConditionStatusTrue
}

// isProvisioned returns true if the node has a Provisioned condition that is true.
func isProvisioned(node db.Node) bool {
	return node.Conditions.Provisioned.Status == db.ConditionStatusTrue
}

// isNodeAgentReady returns true if the node has a NodeAgentReady condition that is true.
func isNodeAgentReady(node db.Node) bool {
	return node.Conditions.NodeAgentReady.Status == db.ConditionStatusTrue
}
