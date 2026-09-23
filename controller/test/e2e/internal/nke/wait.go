// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/dsx-ai-factory/nke/controller/api"
)

const (
	// nodeRebootStartTimeout bounds how long CWE may take to observe a reboot.
	nodeRebootStartTimeout = 5 * time.Minute
	// noLastError is the placeholder used before a poll loop records a failure.
	noLastError = "<none>"
)

// WaitForClusterReady polls the cluster until its Provisioned condition is True,
// erroring if Provisioning flips to False, the deadline passes, or ctx is done.
func (c *Client) WaitForClusterReady(ctx context.Context, id openapi_types.UUID) (*api.Cluster, error) {
	deadline := time.After(c.opts.Provision)
	ticker := time.NewTicker(c.opts.Poll)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return nil, fmt.Errorf("timeout after %v waiting for cluster %s to be provisioned", c.opts.Provision, id)
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for cluster %s to be provisioned: %w", id, ctx.Err())
		case <-ticker.C:
			resp, err := c.api.GetClusterWithResponse(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("getting cluster %s: %w", id, err)
			}
			if resp.StatusCode() == 200 && resp.JSON200 != nil {
				cluster := resp.JSON200
				if cluster.Conditions.Provisioned.Status == api.ConditionStatusTrue {
					return cluster, nil
				}
				if cluster.Conditions.Provisioning.Status == api.ConditionStatusFalse {
					return nil, fmt.Errorf("cluster %s provisioning failed: %s", cluster.Name, cluster.Conditions.Provisioning.Message)
				}
			}
		}
	}
}

// WaitForClusterDeleted polls the cluster until it returns 404, erroring if the
// Deleting condition flips to False, the deadline passes, or ctx is done.
func (c *Client) WaitForClusterDeleted(ctx context.Context, id openapi_types.UUID) error {
	deadline := time.After(c.opts.Delete)
	ticker := time.NewTicker(c.opts.Poll)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return fmt.Errorf("timeout after %v waiting for cluster %s to be deleted", c.opts.Delete, id)
		case <-ctx.Done():
			return fmt.Errorf("waiting for cluster %s to be deleted: %w", id, ctx.Err())
		case <-ticker.C:
			resp, err := c.api.GetClusterWithResponse(ctx, id)
			if err != nil {
				// Transient network error; keep polling until the deadline.
				continue
			}
			if resp.StatusCode() == 404 {
				return nil
			}
			if resp.StatusCode() == 200 && resp.JSON200 != nil {
				cluster := resp.JSON200
				if cluster.Conditions.Deleting.Status == api.ConditionStatusFalse {
					return fmt.Errorf("cluster %s deletion failed: %s", cluster.Name, cluster.Conditions.Deleting.Message)
				}
			}
		}
	}
}

func (c *Client) WaitForEtcdEncryptionEnabled(ctx context.Context, id openapi_types.UUID) (*api.Cluster, error) {
	deadline := time.After(c.opts.Provision)
	ticker := time.NewTicker(c.opts.Poll)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return nil, fmt.Errorf("timeout after %v waiting for cluster %s to enable etcd encryption", c.opts.Provision, id)
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for cluster %s to enable etcd encryption: %w", id, ctx.Err())
		case <-ticker.C:
			cluster, err := c.GetCluster(ctx, id)
			if err != nil {
				continue
			}
			if cluster.Conditions.UpdateFailed != nil && cluster.Conditions.UpdateFailed.Status == api.ConditionStatusTrue {
				return nil, fmt.Errorf("cluster %s etcd encryption enablement failed: %s", cluster.Name, cluster.Conditions.UpdateFailed.Message)
			}
			if cluster.EtcdEncrypted && cluster.Conditions.UpdateInProgress == nil {
				return cluster, nil
			}
		}
	}
}

// WaitForOIDCEnabled polls until the OIDC enablement workflow completes: the
// oidc_enable UpdateInProgress lock is released and no UpdateFailed is recorded.
// The cluster API does not surface the oidc_auth_enabled marker, so callers
// confirm the actual enablement through the tenant-side effects (the
// kubeadm-config write-back and the published tenant-access kubeconfig).
func (c *Client) WaitForOIDCEnabled(ctx context.Context, id openapi_types.UUID) (*api.Cluster, error) {
	deadline := time.After(c.opts.Provision)
	ticker := time.NewTicker(c.opts.Poll)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return nil, fmt.Errorf("timeout after %v waiting for cluster %s to enable OIDC", c.opts.Provision, id)
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for cluster %s to enable OIDC: %w", id, ctx.Err())
		case <-ticker.C:
			cluster, err := c.GetCluster(ctx, id)
			if err != nil {
				continue
			}
			if cluster.Conditions.UpdateFailed != nil && cluster.Conditions.UpdateFailed.Status == api.ConditionStatusTrue {
				return nil, fmt.Errorf("cluster %s OIDC enablement failed: %s", cluster.Name, cluster.Conditions.UpdateFailed.Message)
			}
			if cluster.Conditions.UpdateInProgress == nil {
				return cluster, nil
			}
		}
	}
}

func (c *Client) WaitForProvisionedNodeCount(ctx context.Context, nodePoolID openapi_types.UUID, desired int) error {
	deadline := time.After(c.opts.Provision)
	ticker := time.NewTicker(c.opts.Poll)
	defer ticker.Stop()

	lastProvisionedCount := 0
	lastError := noLastError

	for {
		select {
		case <-deadline:
			return fmt.Errorf("timeout after %v waiting for node pool %s to have %d provisioned nodes (last reported: %d; last error: %s)", c.opts.Provision, nodePoolID, desired, lastProvisionedCount, lastError)
		case <-ctx.Done():
			return fmt.Errorf("waiting for node pool %s to have %d provisioned nodes (last reported: %d; last error: %s): %w", nodePoolID, desired, lastProvisionedCount, lastError, ctx.Err())
		case <-ticker.C:
			nodes, err := c.ListNodePoolNodes(ctx, nodePoolID)
			if err != nil {
				lastError = err.Error()
				continue // ignore potentially transient errors
			}
			provisioned := 0
			for _, node := range nodes {
				if node.Conditions.Provisioned.Status == api.ConditionStatusTrue {
					provisioned++
				}
			}
			lastProvisionedCount = provisioned
			if provisioned == desired {
				return nil
			}
		}
	}
}

// WaitForNodePoolDeleted polls the node pool until GetNodePool returns 404,
// erroring if the deadline passes or ctx is done.
func (c *Client) WaitForNodePoolDeleted(ctx context.Context, id openapi_types.UUID) error {
	deadline := time.After(c.opts.Delete)
	ticker := time.NewTicker(c.opts.Poll)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return fmt.Errorf("timeout after %v waiting for node pool %s to be deleted", c.opts.Delete, id)
		case <-ctx.Done():
			return fmt.Errorf("waiting for node pool %s to be deleted: %w", id, ctx.Err())
		case <-ticker.C:
			resp, err := c.api.GetNodePoolWithResponse(ctx, id)
			if err != nil {
				// Transient network error; keep polling until the deadline.
				continue
			}
			switch resp.StatusCode() {
			case 404:
				return nil
			case 200:
				// Node pool still exists; keep polling until it is gone.
			default:
				// Persistent auth/server errors should surface immediately
				// rather than as an opaque timeout.
				return fmt.Errorf("waiting for node pool %s to be deleted: unexpected status %s: %s", id, resp.Status(), resp.Body)
			}
		}
	}
}

// WaitForProvisionedNodeCountExcluding waits until the node pool has exactly
// desired provisioned nodes and the excluded node ID is gone. Unlike
// WaitForProvisionedNodeCount it asserts a deleted node was actually replaced,
// not merely that the count recovered while the deleted node still lingers.
func (c *Client) WaitForProvisionedNodeCountExcluding(ctx context.Context, nodePoolID openapi_types.UUID, desired int, excluded openapi_types.UUID) error {
	deadline := time.After(c.opts.Provision)
	ticker := time.NewTicker(c.opts.Poll)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return fmt.Errorf("timeout after %v waiting for node pool %s to have %d provisioned nodes without node %s", c.opts.Provision, nodePoolID, desired, excluded)
		case <-ctx.Done():
			return fmt.Errorf("waiting for node pool %s replacement: %w", nodePoolID, ctx.Err())
		case <-ticker.C:
			nodes, err := c.ListNodePoolNodes(ctx, nodePoolID)
			if err != nil {
				continue
			}
			provisioned := 0
			excludedSeen := false
			for _, node := range nodes {
				if node.Conditions.Provisioned.Status == api.ConditionStatusTrue {
					provisioned++
				}
				if node.ID == excluded {
					excludedSeen = true
				}
			}
			if provisioned == desired && !excludedSeen {
				return nil
			}
		}
	}
}

// WaitForProvisionedNodeCountStable waits until the node pool holds exactly
// desired total (all provisioned) nodes continuously for stableFor. Used to
// assert CWE does not spuriously re-provision a manually deleted node after the
// desired count was lowered (a delayed replacement resets the stability window).
func (c *Client) WaitForProvisionedNodeCountStable(ctx context.Context, nodePoolID openapi_types.UUID, desired int, stableFor time.Duration) error {
	deadline := time.After(c.opts.Provision)
	ticker := time.NewTicker(c.opts.Poll)
	defer ticker.Stop()

	var stableSince time.Time
	for {
		select {
		case <-deadline:
			return fmt.Errorf("timeout after %v waiting for node pool %s to hold %d provisioned nodes stable for %v", c.opts.Provision, nodePoolID, desired, stableFor)
		case <-ctx.Done():
			return fmt.Errorf("waiting for node pool %s to be stable: %w", nodePoolID, ctx.Err())
		case <-ticker.C:
			nodes, err := c.ListNodePoolNodes(ctx, nodePoolID)
			if err != nil {
				stableSince = time.Time{}
				continue
			}
			provisioned := 0
			for _, node := range nodes {
				if node.Conditions.Provisioned.Status == api.ConditionStatusTrue {
					provisioned++
				}
			}
			if len(nodes) == desired && provisioned == desired {
				if stableSince.IsZero() {
					stableSince = time.Now()
				}
				if time.Since(stableSince) >= stableFor {
					return nil
				}
			} else {
				stableSince = time.Time{}
			}
		}
	}
}

// WaitForControlPlaneNodesRolled polls until the controller-managed control-plane
// node set (NodePoolID == uuid.Nil) contains exactly desired KubernetesReady
// nodes and none of oldNodeIDs remain. It asserts a config-revision rolling
// replacement fully rolled the control plane onto new nodes at the same desired
// count, rather than merely that the count recovered.
func (c *Client) WaitForControlPlaneNodesRolled(ctx context.Context, clusterID openapi_types.UUID, desired int, oldNodeIDs map[openapi_types.UUID]struct{}) error {
	deadline := time.After(c.opts.Provision)
	ticker := time.NewTicker(c.opts.Poll)
	defer ticker.Stop()

	lastError := noLastError
	for {
		select {
		case <-deadline:
			return fmt.Errorf("timeout after %v waiting for control-plane roll on cluster %s (last error: %s)", c.opts.Provision, clusterID, lastError)
		case <-ctx.Done():
			return fmt.Errorf("waiting for control-plane roll on cluster %s (last error: %s): %w", clusterID, lastError, ctx.Err())
		case <-ticker.C:
			nodes, err := c.ListClusterNodes(ctx, clusterID)
			if err != nil {
				lastError = err.Error()
				continue
			}
			controlPlaneNodes := make([]api.Node, 0, desired)
			for _, node := range nodes {
				if node.NodePoolID == uuid.Nil {
					controlPlaneNodes = append(controlPlaneNodes, node)
				}
			}
			if len(controlPlaneNodes) != desired {
				lastError = fmt.Sprintf("%d control-plane nodes present; want %d", len(controlPlaneNodes), desired)
				continue
			}
			if err := controlPlaneNodesRolled(controlPlaneNodes, oldNodeIDs); err != nil {
				lastError = err.Error()
				continue
			}
			return nil
		}
	}
}

// controlPlaneNodesRolled reports an error unless every node in controlPlaneNodes
// is absent from oldNodeIDs and KubernetesReady.
func controlPlaneNodesRolled(controlPlaneNodes []api.Node, oldNodeIDs map[openapi_types.UUID]struct{}) error {
	for _, node := range controlPlaneNodes {
		if _, old := oldNodeIDs[node.ID]; old {
			return fmt.Errorf("old control-plane node %q still present", node.Name)
		}
		if ready, reason := IsNodeReady(node); !ready {
			return fmt.Errorf("replacement control-plane node %q not ready: %s", node.Name, reason)
		}
	}
	return nil
}

// WaitForControlPlaneUpgradeComplete polls until the cluster reports the target
// control-plane version and upgrade/recycling conditions are no longer active.
func (c *Client) WaitForControlPlaneUpgradeComplete(ctx context.Context, id openapi_types.UUID, targetVersion string) (*api.Cluster, error) {
	deadline := time.After(c.opts.Upgrade)
	ticker := time.NewTicker(c.opts.Poll)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return nil, fmt.Errorf("timeout after %v waiting for cluster %s control-plane upgrade to %s", c.opts.Upgrade, id, targetVersion)
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for cluster %s control-plane upgrade to %s: %w", id, targetVersion, ctx.Err())
		case <-ticker.C:
			cluster, err := c.GetCluster(ctx, id)
			if err != nil {
				continue
			}

			if cluster.Conditions.UpdateFailed != nil && cluster.Conditions.UpdateFailed.Status == api.ConditionStatusTrue {
				return nil, fmt.Errorf("cluster %s control-plane upgrade failed: %s", cluster.Name, cluster.Conditions.UpdateFailed.Message)
			}
			if cluster.ControlPlaneKubernetesVersion != targetVersion {
				continue
			}
			if conditionIsTrue(cluster.Conditions.ControlPlaneUpgrading) {
				continue
			}
			if conditionIsTrue(cluster.Conditions.CpNodesRecycling) {
				continue
			}

			return cluster, nil
		}
	}
}

func conditionIsTrue(condition *api.Condition) bool {
	return condition != nil && condition.Status == api.ConditionStatusTrue
}

// WaitForReadyNodeInPool polls until some node in the pool reports
// KubernetesReady=True and returns it. A cluster reporting Provisioned=True does
// not imply its worker nodes have joined yet, so callers that need a live node
// must wait rather than sample once.
func (c *Client) WaitForReadyNodeInPool(ctx context.Context, nodePoolID openapi_types.UUID) (*api.Node, error) {
	deadline := time.After(c.opts.Provision)
	ticker := time.NewTicker(c.opts.Poll)
	defer ticker.Stop()

	lastError := noLastError
	for {
		select {
		case <-deadline:
			return nil, fmt.Errorf("timeout after %v waiting for a KubernetesReady node in pool %s (last error: %s)", c.opts.Provision, nodePoolID, lastError)
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for a KubernetesReady node in pool %s: %w", nodePoolID, ctx.Err())
		case <-ticker.C:
			nodes, err := c.ListNodePoolNodes(ctx, nodePoolID)
			if err != nil {
				lastError = err.Error()
				continue
			}
			for _, node := range nodes {
				if node.Conditions.KubernetesReady.Status == api.ConditionStatusTrue {
					return &node, nil
				}
			}
			lastError = fmt.Sprintf("none of %d nodes report KubernetesReady=True", len(nodes))
		}
	}
}

// WaitForNodeRebooting polls until the node's Rebooting condition is True, or
// until it reports a reboot that already finished after since: on a container
// backend the True window can close between two polls.
func (c *Client) WaitForNodeRebooting(ctx context.Context, id openapi_types.UUID, since time.Time) (*api.Node, error) {
	deadline := time.After(nodeRebootStartTimeout)
	ticker := time.NewTicker(c.opts.Poll)
	defer ticker.Stop()

	var lastStatus, lastMessage string
	for {
		select {
		case <-deadline:
			return nil, fmt.Errorf("timeout after %v waiting for node %s rebooting=True (last: %s (%s))", nodeRebootStartTimeout, id, lastStatus, lastMessage)
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for node %s rebooting=True: %w", id, ctx.Err())
		case <-ticker.C:
			node, err := c.GetNode(ctx, id)
			if err != nil {
				continue
			}
			rebooting := node.Conditions.Rebooting
			lastStatus = string(rebooting.Status)
			lastMessage = rebooting.Message
			if rebooting.Status == api.ConditionStatusTrue ||
				(rebooting.Status == api.ConditionStatusFalse && rebooting.LastTransitionTime.After(since)) {
				return node, nil
			}
		}
	}
}

// WaitForNodeRebootComplete polls until Rebooting is False and KubernetesReady
// is True — the node finished remediation and rejoined healthy.
func (c *Client) WaitForNodeRebootComplete(ctx context.Context, id openapi_types.UUID) (*api.Node, error) {
	deadline := time.After(c.opts.Provision)
	ticker := time.NewTicker(c.opts.Poll)
	defer ticker.Stop()

	var lastRebooting, lastReady string
	for {
		select {
		case <-deadline:
			return nil, fmt.Errorf("timeout after %v waiting for node %s reboot to complete (rebooting=%s ready=%s)", c.opts.Provision, id, lastRebooting, lastReady)
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for node %s reboot to complete: %w", id, ctx.Err())
		case <-ticker.C:
			node, err := c.GetNode(ctx, id)
			if err != nil {
				continue
			}
			lastRebooting = string(node.Conditions.Rebooting.Status)
			lastReady = string(node.Conditions.KubernetesReady.Status)
			if node.Conditions.Rebooting.Status == api.ConditionStatusFalse &&
				node.Conditions.KubernetesReady.Status == api.ConditionStatusTrue {
				return node, nil
			}
		}
	}
}
