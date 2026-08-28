// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	"context"
	"fmt"
	"slices"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/NVIDIA/nke/controller/api"
)

// NodeCondition evaluates one required property of a CWE node. When it returns
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
			return nil, fmt.Errorf("CWE does not report expected node pool %q", poolName)
		}

		pool := nodePools[poolIndex]
		if pool.DesiredNodeCount != expectedNodeCount {
			return nil, fmt.Errorf(
				"CWE node pool %q has desired count %d; want %d",
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
				"CWE node pool %q reports %d nodes; want %d",
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

// IsNodeReady reports whether CWE's KubernetesReady condition is true.
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
