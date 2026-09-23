// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/nodes"
)

// controlPlane represents a control plane in the database.
// It is a read-only view of the control plane nodes and node pools.
type controlPlane struct {
	nodes     []db.Node
	nodePools []nodePool
	nodeSets  []db.NodeSet
	// nodePinnedControlPlaneConfigs maps each CP-controller node's pinned config ID
	// to its spec, preloaded once per reconcile.
	nodePinnedControlPlaneConfigs map[uuid.UUID]db.ControlPlaneConfigSpec
	clusterControlPlaneConfig     db.ControlPlaneConfig
	useController                 bool
}

// readControlPlane reads the control plane from the database.
func readControlPlane(ctx context.Context, database db.Queries, cluster db.Cluster, useController bool) (*controlPlane, error) {
	cpTrue := true
	nodes, err := database.ListNodes(ctx, db.NodeFilters{
		ClusterID:    &cluster.ID,
		ControlPlane: &cpTrue,
	})
	if err != nil {
		return nil, fmt.Errorf("listing control plane nodes: %w", err)
	}

	dbNodePools, err := database.ListNodePools(ctx, db.NodePoolFilters{
		ClusterID:    &cluster.ID,
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

	// Preload CP-controller nodes' pinned config snapshots (batched).
	configs, err := preloadControlPlaneConfigs(ctx, database, nodes)
	if err != nil {
		return nil, err
	}

	return &controlPlane{
		nodes:                         nodes,
		nodePools:                     nodePools,
		nodeSets:                      allNodeSets,
		nodePinnedControlPlaneConfigs: configs,
		clusterControlPlaneConfig:     cluster.ControlPlaneConfig,
		useController:                 useController,
	}, nil
}

// preloadControlPlaneConfigs batch-loads the pinned configuration for the distinct
// config IDs referenced by CP-controller nodes.
func preloadControlPlaneConfigs(
	ctx context.Context,
	database db.Queries,
	nodes []db.Node,
) (map[uuid.UUID]db.ControlPlaneConfigSpec, error) {
	ids := make([]uuid.UUID, 0, len(nodes))
	seen := make(map[uuid.UUID]struct{}, len(nodes))
	for _, node := range nodes {
		if !node.IsManagedByControlPlaneController() || node.ControlPlaneConfigurationID == uuid.Nil {
			continue
		}
		if _, ok := seen[node.ControlPlaneConfigurationID]; ok {
			continue
		}
		seen[node.ControlPlaneConfigurationID] = struct{}{}
		ids = append(ids, node.ControlPlaneConfigurationID)
	}
	if len(ids) == 0 {
		return map[uuid.UUID]db.ControlPlaneConfigSpec{}, nil
	}
	rows, err := database.ListControlPlaneConfigurationsByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("listing control plane configuration snapshots: %w", err)
	}
	configs := make(map[uuid.UUID]db.ControlPlaneConfigSpec, len(rows))
	for _, row := range rows {
		configs[row.ID] = row.Config
	}
	return configs, nil
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

// surgeBudget is how many surge nodes the rollout may run concurrently,
// bounded by the number of nodes still needing rollout.
func (c *controlPlane) surgeBudget() int {
	return min(c.clusterControlPlaneConfig.MaxSurge,
		len(c.nodesNeedingRollout(c.clusterControlPlaneConfig.ControlPlaneConfigSpec)))
}

// expectedCount returns the expected number of control plane nodes.
func (c *controlPlane) expectedCount() int {
	if c.useController {
		return c.clusterControlPlaneConfig.DesiredCount + c.surgeBudget()
	}
	total := 0
	for _, np := range c.nodePools {
		total += np.expectedCount()
	}
	return total
}

// countDesiredNodes returns the total desired number of control plane nodes (without surge).
func (c *controlPlane) countDesiredNodes() int {
	if c.useController {
		return c.clusterControlPlaneConfig.DesiredCount
	}
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

// countNodesNeedingRollout returns the total count of control plane nodes
// needing rollout across all control plane node pools.
func (c *controlPlane) countNodesNeedingRollout() int {
	if c.useController {
		return len(c.nodesNeedingRollout(c.clusterControlPlaneConfig.ControlPlaneConfigSpec))
	}
	total := 0
	for _, np := range c.nodePools {
		total += np.countDeprecatedNodes()
	}
	return total
}

// nodesOffVersion returns the node-pool nodes whose NodeSet pins a Kubernetes
// version other than target, their pools, and whether any such pool has
// auto-provision disabled. Those pools never get a new NodeSet, so the count
// cannot fall on its own.
func (c *controlPlane) nodesOffVersion(target string) (int, []uuid.UUID, bool) {
	count := 0
	stuck := false
	var pools []uuid.UUID
	for _, np := range c.nodePools {
		poolCount := 0
		for _, ns := range np.nodeSets {
			if ns.KubernetesVersion != "" && ns.KubernetesVersion != target {
				poolCount += len(ns.nodes)
			}
		}
		if poolCount == 0 {
			continue
		}
		count += poolCount
		pools = append(pools, np.ID)
		if np.AutoProvisionDisabled {
			stuck = true
		}
	}
	return count, pools, stuck
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

// nodesNeedingRollout returns CP-controller nodes whose pinned snapshot differs
// from current in a field that requires replacement (k8s version, OS profile, or
// config profile).
func (c *controlPlane) nodesNeedingRollout(current db.ControlPlaneConfigSpec) []db.Node {
	var toReplace []db.Node
	for _, node := range c.nodes {
		if !node.IsManagedByControlPlaneController() {
			continue
		}
		pinned, ok := c.nodePinnedControlPlaneConfigs[node.ControlPlaneConfigurationID]
		if !ok {
			continue
		}
		if db.IsControlPlaneRolloutRequired(pinned, current) {
			toReplace = append(toReplace, node)
		}
	}
	return toReplace
}

// controlPlaneRolloutRequiredMessage renders the condition message from the diff,
// so it never re-enumerates fields (db.Condition has no Reason field).
func controlPlaneRolloutRequiredMessage(changes []db.ControlPlaneRolloutChange) string {
	if len(changes) == 0 {
		return "Pinned control-plane configuration matches the cluster's desired configuration"
	}
	parts := make([]string, len(changes))
	for i, c := range changes {
		parts[i] = fmt.Sprintf("%s %s->%s", c.Field, c.From, c.To)
	}
	return "Pinned control-plane configuration differs from desired: " + strings.Join(parts, ", ")
}

// refreshControlPlaneRolloutRequiredCondition recomputes the node's
// ControlPlaneRolloutRequired condition from its pinned snapshot versus the
// cluster's desired snapshot, transitioning LastTransitionTime only on a status
// flip (via db.SetCondition). The condition is True when the node has drifted and
// needs a roll, False when up to date. It returns true when the condition changed
// and the node must be persisted. Nodes not managed by the control-plane
// controller, or whose pinned snapshot was not preloaded, are left untouched.
func (c *controlPlane) refreshControlPlaneRolloutRequiredCondition(node *db.Node) bool {
	if !node.IsManagedByControlPlaneController() {
		return false
	}
	pinned, ok := c.nodePinnedControlPlaneConfigs[node.ControlPlaneConfigurationID]
	if !ok {
		return false
	}
	changes := db.ComputeControlPlaneRolloutChanges(pinned, c.clusterControlPlaneConfig.ControlPlaneConfigSpec)

	previous := node.Conditions.ControlPlaneRolloutRequired
	db.SetCondition(&node.Conditions.ControlPlaneRolloutRequired, db.Condition{
		Status:  db.ConditionStatusFromBool(len(changes) > 0),
		Message: controlPlaneRolloutRequiredMessage(changes),
	})
	return node.Conditions.ControlPlaneRolloutRequired != previous
}

// controlPlaneControllerNeedsK8sUpgrade reports whether any CP-controller node's
// pinned snapshot k8s version differs from the target (drives kubeadm + addons).
func (c *controlPlane) controlPlaneControllerNeedsK8sUpgrade(targetK8sVersion string) bool {
	for _, node := range c.nodes {
		if !node.IsManagedByControlPlaneController() {
			continue
		}
		pinned, ok := c.nodePinnedControlPlaneConfigs[node.ControlPlaneConfigurationID]
		if !ok {
			continue
		}
		if pinned.KubernetesVersion != targetK8sVersion {
			return true
		}
	}
	return false
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

// nodesWithDeleteRequested returns the nodes whose delete has been accepted but
// whose teardown has not started yet.
func (c *controlPlane) nodesWithDeleteRequested() []db.Node {
	var requested []db.Node
	for _, node := range c.nodes {
		if nodes.IsDeleteRequested(node) {
			requested = append(requested, node)
		}
	}
	return requested
}

// nodeTearingDown returns the first node whose teardown is in flight, if any.
func (c *controlPlane) nodeTearingDown() (db.Node, bool) {
	for _, node := range c.nodes {
		if isMarkedForDeletion(node) {
			return node, true
		}
	}
	return db.Node{}, false
}

// nodeWithDeleteInFlight returns the first node with an accepted delete that has
// not finished, recorded or already tearing down, and whether there was one.
func (c *controlPlane) nodeWithDeleteInFlight() (db.Node, bool) {
	for _, node := range c.nodes {
		if nodes.IsDeleteInFlight(node) {
			return node, true
		}
	}
	return db.Node{}, false
}

// hasDeleteInFlight reports whether any control-plane node has an accepted
// delete that has not finished: recorded and waiting, or already tearing down.
// Anything that would add or replace a control-plane node has to wait on this,
// so a join never overlaps an etcd member removal.
func (c *controlPlane) hasDeleteInFlight() bool {
	_, ok := c.nodeWithDeleteInFlight()
	return ok
}
