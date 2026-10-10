// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/dsx-ai-factory/nke/controller/api"
)

// NodeCondition evaluates one required property of a node reported by the controller. When it returns
// false, reason is included in the assertion diagnostics.
type NodeCondition func(api.Node) (bool, string)

// AssertNodeConditions checks every requested condition on the nodes belonging
// to expectedNodePools. The caller controls retries.
func (c *Client) AssertNodeConditions(
	ctx context.Context,
	clusterID openapi_types.UUID,
	expectedNodePools map[string]int,
	conditions ...NodeCondition,
) error {
	for i, condition := range conditions {
		if condition == nil {
			return fmt.Errorf("asserting node conditions for cluster %s: condition %d is nil", clusterID, i+1)
		}
	}

	nodes, err := c.ListExpectedNodes(ctx, clusterID, expectedNodePools)
	if err != nil {
		return fmt.Errorf("asserting node conditions for cluster %s: %w", clusterID, err)
	}
	for _, node := range nodes {
		for i, condition := range conditions {
			matched, reason := condition(node)
			if matched {
				continue
			}
			if reason == "" {
				reason = fmt.Sprintf("condition %d not met", i+1)
			}
			return fmt.Errorf("node conditions not met for cluster %s: node %q: %s", clusterID, node.Name, reason)
		}
	}
	return nil
}

// CollectNodeDiagnostics is what a failed run has instead of a machine it can
// log into: the cluster and every one of its nodes, conditions included, as
// JSON.
//
// Separate from AssertNodeConditions because the two want opposite things. An
// assertion stops at the first node that fails, which is right for deciding
// pass or fail and wrong for a post-mortem: a pool reports one node and the
// others go unrecorded. This stops at nothing.
//
// It also runs where the assertion cannot. When provisioning times out the
// readiness spec never executes, which is the failure this lane actually hits,
// so the only diagnostic it had was unreachable exactly when it was needed.
//
// Best effort throughout. It is called from a path that is already failing,
// and a collector that fails the run it was trying to explain is worse than
// one that returns what it managed to read.
func (c *Client) CollectNodeDiagnostics(ctx context.Context, clusterID openapi_types.UUID) []byte {
	report := struct {
		ClusterID string         `json:"clusterID"`
		Cluster   *api.Cluster   `json:"cluster,omitempty"`
		NodePools []api.NodePool `json:"nodePools,omitempty"`
		Nodes     []api.Node     `json:"nodes,omitempty"`
		Errors    []string       `json:"errors,omitempty"`
	}{ClusterID: clusterID.String()}

	if cluster, err := c.GetCluster(ctx, clusterID); err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("reading the cluster: %v", err))
	} else {
		report.Cluster = cluster
	}

	// The pools as well as the nodes: a pool short of its desired count is the
	// case worth seeing, and a list of the nodes that exist cannot show the
	// ones that do not.
	if pools, err := c.ListNodePools(ctx, clusterID); err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("listing the node pools: %v", err))
	} else {
		report.NodePools = pools
	}

	// Every node the cluster has, not the ones a caller expected: the
	// difference is the finding.
	if nodes, err := c.ListClusterNodes(ctx, clusterID); err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("listing the nodes: %v", err))
	} else {
		report.Nodes = nodes
	}

	out, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return []byte(fmt.Sprintf("{\"clusterID\": %q, \"errors\": [%q]}", clusterID.String(), err.Error()))
	}
	return out
}

// ListExpectedNodes returns the cluster nodes that belong to expectedNodePools
// after verifying each pool's desired and actual node counts.
func (c *Client) ListExpectedNodes(
	ctx context.Context,
	clusterID openapi_types.UUID,
	expectedNodePools map[string]int,
) ([]api.Node, error) {
	nodePools, err := c.ListNodePools(ctx, clusterID)
	if err != nil {
		return nil, err
	}

	type expectedPool struct {
		name      string
		nodeCount int
	}
	expectedPoolsByID := make(map[openapi_types.UUID]expectedPool, len(expectedNodePools))
	for poolName, expectedNodeCount := range expectedNodePools {
		poolIndex := slices.IndexFunc(nodePools, func(pool api.NodePool) bool {
			return pool.Name == poolName
		})
		if poolIndex < 0 {
			return nil, fmt.Errorf("controller does not report expected node pool %q", poolName)
		}

		pool := nodePools[poolIndex]
		if pool.DesiredNodeCount != expectedNodeCount {
			return nil, fmt.Errorf(
				"controller node pool %q has desired count %d; want %d",
				poolName, pool.DesiredNodeCount, expectedNodeCount,
			)
		}
		expectedPoolsByID[pool.ID] = expectedPool{name: poolName, nodeCount: expectedNodeCount}
	}

	clusterNodes, err := c.ListClusterNodes(ctx, clusterID)
	if err != nil {
		return nil, err
	}

	expectedNodes := make([]api.Node, 0)
	nodeCountsByPoolID := make(map[openapi_types.UUID]int, len(expectedPoolsByID))
	for _, node := range clusterNodes {
		if _, expected := expectedPoolsByID[node.NodePoolID]; !expected {
			continue
		}
		expectedNodes = append(expectedNodes, node)
		nodeCountsByPoolID[node.NodePoolID]++
	}

	for poolID, pool := range expectedPoolsByID {
		if actualCount := nodeCountsByPoolID[poolID]; actualCount != pool.nodeCount {
			return nil, fmt.Errorf(
				"controller node pool %q reports %d nodes; want %d",
				pool.name, actualCount, pool.nodeCount,
			)
		}
	}

	return expectedNodes, nil
}

// AssertControlPlaneNodesReady verifies that exactly desired control-plane nodes
// are present and KubernetesReady, from the cluster control-plane resource's
// aggregate status (readyNodeCount counts control-plane nodes whose
// KubernetesReady condition is true). The caller controls retries.
func (c *Client) AssertControlPlaneNodesReady(ctx context.Context, clusterID openapi_types.UUID, desired int) error {
	cp, err := c.GetClusterControlPlane(ctx, clusterID)
	if err != nil {
		return fmt.Errorf("asserting control-plane nodes for cluster %s: %w", clusterID, err)
	}
	if cp.Status.NodeCount != desired {
		return fmt.Errorf("cluster %s has %d control-plane nodes; want %d", clusterID, cp.Status.NodeCount, desired)
	}
	if cp.Status.ReadyNodeCount != desired {
		return fmt.Errorf("cluster %s has %d control-plane nodes KubernetesReady; want %d", clusterID, cp.Status.ReadyNodeCount, desired)
	}
	return nil
}

// ControlPlaneNodeNames returns the names of the cluster's control-plane nodes,
// from the ListNodes controlPlane filter. Callers use the names to cross-check
// the same nodes in the tenant Kubernetes API.
func (c *Client) ControlPlaneNodeNames(ctx context.Context, clusterID openapi_types.UUID) ([]string, error) {
	nodes, err := c.ListClusterNodesByControlPlane(ctx, clusterID, true)
	if err != nil {
		return nil, fmt.Errorf("listing control-plane node names for cluster %s: %w", clusterID, err)
	}

	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		names = append(names, node.Name)
	}
	return names, nil
}

// ControlPlaneNodeIDs returns the IDs of the cluster's control-plane nodes as a
// set, from the ListNodes controlPlane filter. It captures the pre-roll node set
// so a config-revision rolling replacement can be detected by comparing against
// the post-roll set.
func (c *Client) ControlPlaneNodeIDs(ctx context.Context, clusterID openapi_types.UUID) (map[openapi_types.UUID]struct{}, error) {
	nodes, err := c.ListClusterNodesByControlPlane(ctx, clusterID, true)
	if err != nil {
		return nil, fmt.Errorf("listing control-plane node IDs for cluster %s: %w", clusterID, err)
	}

	ids := make(map[openapi_types.UUID]struct{}, len(nodes))
	for _, node := range nodes {
		ids[node.ID] = struct{}{}
	}
	return ids, nil
}

// IsNodeReady reports whether the controller's KubernetesReady condition is true.
func IsNodeReady(node api.Node) (bool, string) {
	condition := node.Conditions.KubernetesReady
	if condition.Status == api.ConditionStatusTrue {
		return true, ""
	}

	status := string(condition.Status)
	if status == "" {
		status = "Unset"
	}
	detail := "KubernetesReady=" + status
	if condition.Message != "" {
		detail += fmt.Sprintf(" (%q)", condition.Message)
	}
	return false, detail
}

// DeleteNodeExpectingRefusal requires DeleteNode to be refused with a 400 and
// returns the API's message, so a spec can assert on why it was refused rather
// than only that something failed.
func (c *Client) DeleteNodeExpectingRefusal(ctx context.Context, id openapi_types.UUID, opts ...DeleteNodeOption) (string, error) {
	req := api.DeleteNodeRequest{}
	for _, opt := range opts {
		opt(&req)
	}
	resp, err := c.api.DeleteNodeWithResponse(ctx, id, req)
	if err != nil {
		return "", fmt.Errorf("deleting node %s: %w", id, err)
	}
	if resp.StatusCode() != 400 || resp.JSON400 == nil {
		return "", fmt.Errorf("deleting node %s: expected the delete to be refused with 400, got %s: %s", id, resp.Status(), resp.Body)
	}
	return resp.JSON400.Message, nil
}

// ControlPlaneDeleteState is a point-in-time view of control-plane deletion for a
// cluster: the cluster-level counters plus the joined node set the join gate must
// hold still while a delete is in flight.
type ControlPlaneDeleteState struct {
	// Deleting is the number of control-plane nodes whose teardown is running.
	Deleting int
	// DeleteRequested is the number whose delete was accepted but not started.
	DeleteRequested int
	// JoinedNodeIDs is the set of control-plane nodes reporting Joined.
	JoinedNodeIDs map[openapi_types.UUID]struct{}
	// NodeIDs is the set of control-plane nodes that exist at all.
	NodeIDs map[openapi_types.UUID]struct{}
	// ReadyNodeCount is how many control-plane nodes are KubernetesReady.
	ReadyNodeCount int
}

// InFlight reports whether any control-plane delete has been accepted and not
// yet finished, whether it is running or still queued behind another.
func (s ControlPlaneDeleteState) InFlight() bool {
	return s.Deleting > 0 || s.DeleteRequested > 0
}

// ControlPlaneDeleteStateAt samples the cluster's control-plane delete state from
// the control-plane read API plus the control-plane node list.
func (c *Client) ControlPlaneDeleteStateAt(ctx context.Context, clusterID openapi_types.UUID) (ControlPlaneDeleteState, error) {
	cp, err := c.GetClusterControlPlane(ctx, clusterID)
	if err != nil {
		return ControlPlaneDeleteState{}, fmt.Errorf("reading control-plane delete state for cluster %s: %w", clusterID, err)
	}

	nodes, err := c.ListClusterNodesByControlPlane(ctx, clusterID, true)
	if err != nil {
		return ControlPlaneDeleteState{}, fmt.Errorf("listing control-plane nodes for cluster %s: %w", clusterID, err)
	}

	state := ControlPlaneDeleteState{
		Deleting:        cp.Status.DeletingNodeCount,
		DeleteRequested: cp.Status.DeleteRequestedNodeCount,
		ReadyNodeCount:  cp.Status.ReadyNodeCount,
		JoinedNodeIDs:   make(map[openapi_types.UUID]struct{}, len(nodes)),
		NodeIDs:         make(map[openapi_types.UUID]struct{}, len(nodes)),
	}
	for _, node := range nodes {
		state.NodeIDs[node.ID] = struct{}{}
		if node.Conditions.Joined.Status == api.ConditionStatusTrue {
			state.JoinedNodeIDs[node.ID] = struct{}{}
		}
	}
	return state, nil
}

// WaitForJoinedControlPlaneNodes polls until the cluster reports exactly desired
// control-plane nodes and all of them report Joined.
//
// AssertControlPlaneNodesReady is not a substitute. KubernetesReady is written by
// the tenant node-status controller as soon as the kubelet registers and goes
// Ready, while Joined is written by a later activity in the ProvisionNode
// workflow, so a node can be KubernetesReady with Joined not yet committed. Any
// spec whose next step depends on the joined count -- as the control-plane quorum
// floor does -- has to wait on Joined, or it races that window.
func (c *Client) WaitForJoinedControlPlaneNodes(ctx context.Context, clusterID openapi_types.UUID, desired int) error {
	deadline := time.After(c.opts.Provision)
	ticker := time.NewTicker(c.opts.Poll)
	defer ticker.Stop()

	lastError := noLastError
	for {
		select {
		case <-deadline:
			return fmt.Errorf("timeout after %v waiting for %d joined control-plane nodes on cluster %s (last error: %s)",
				c.opts.Provision, desired, clusterID, lastError)
		case <-ctx.Done():
			return fmt.Errorf("waiting for %d joined control-plane nodes on cluster %s (last error: %s): %w",
				desired, clusterID, lastError, ctx.Err())
		case <-ticker.C:
			state, err := c.ControlPlaneDeleteStateAt(ctx, clusterID)
			if err != nil {
				lastError = err.Error()
				continue
			}
			if len(state.NodeIDs) != desired {
				lastError = fmt.Sprintf("%d control-plane nodes present; want %d", len(state.NodeIDs), desired)
				continue
			}
			if len(state.JoinedNodeIDs) != desired {
				lastError = fmt.Sprintf("%d control-plane nodes report Joined; want %d", len(state.JoinedNodeIDs), desired)
				continue
			}
			return nil
		}
	}
}
