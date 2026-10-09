// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nke/controller/internal/clients"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/nodes"
)

const (
	defaultControlPlaneNodePrefix = "cp"
)

// scaleUp creates pending control-plane nodes and returns the number created.
// An allocator seeded with eligible domains spreads new nodes across them. A
// zero-value allocator leaves the requested domain empty for legacy node-pool
// surge nodes. Instance-type selection remains the control plane node
// scheduler's responsibility.
func (r *reconciler) scaleUp(
	ctx context.Context,
	cluster db.Cluster,
	count int,
	reason string,
	allocator domainAllocator,
	newNodeProvider clients.NewNodeProvider,
) (int, error) {
	if count <= 0 {
		return 0, nil
	}

	existingNodeNames, err := r.db.ListClusterNodeNames(ctx, cluster.ID)
	if err != nil {
		return 0, fmt.Errorf("listing cluster node names: %w", err)
	}

	created := 0
	for range count {
		failureDomain := allocator.take()
		createdNode, err := r.createPendingNode(ctx, cluster, existingNodeNames, failureDomain, newNodeProvider)
		if err != nil {
			return created, err
		}
		existingNodeNames = append(existingNodeNames, createdNode.Name)
		created++

		r.logger.Info(
			"control-plane scale-up from control_plane_config completed",
			"clusterID", cluster.ID,
			"clusterName", cluster.Name,
			"nodeName", createdNode.Name,
			"reason", reason,
		)
	}

	return created, nil
}

func (r *reconciler) createPendingNode(
	ctx context.Context,
	cluster db.Cluster,
	existingNodeNames []string,
	failureDomain string,
	newNodeProvider clients.NewNodeProvider,
) (db.Node, error) {
	if cluster.ControlPlaneConfig.ConfigurationID == uuid.Nil {
		return db.Node{}, fmt.Errorf("missing control plane configuration ID for pending control-plane node")
	}

	pendingNode := db.Node{
		ClusterID:                   cluster.ID,
		ControlPlane:                true,
		ControlPlaneConfigurationID: cluster.ControlPlaneConfig.ConfigurationID,
		Name:                        generateControlPlaneNodeName(existingNodeNames, cluster.Name, defaultControlPlaneNodePrefix),
		FailureDomain:               failureDomain,
		Conditions: db.NodeConditions{
			Pending:                 db.NewCondition(true, "Pending control-plane node provisioning"),
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
	nodes.PinAgentProfile(&pendingNode, r.cfg)

	newNodeProvider.ApplyPlaceholderNodePayload(&pendingNode)

	createdNode, err := r.db.CreateNode(ctx, &pendingNode)
	if err != nil {
		return db.Node{}, fmt.Errorf("creating pending control-plane node: %w", err)
	}
	return createdNode, nil
}

// deleteNode tears down a control-plane node after verifying
// cluster health. Returns true when the node was marked for deletion.
func (r *reconciler) deleteNode(
	ctx context.Context,
	cluster db.Cluster,
	cp *controlPlane,
	node db.Node,
	reason string,
	logger *slog.Logger,
) (bool, error) {
	// The in-flight check lives in teardownOldNode, against the control plane this
	// reconcile already loaded. Health is this caller's own precondition.
	healthy, err := r.isControlPlaneHealthy(ctx, cluster, cp, logger)
	if err != nil {
		return false, err
	}
	if !healthy {
		logger.Info("Control plane not ready for node delete - waiting for sufficient healthy nodes",
			"nodeName", node.Name,
		)
		return false, nil
	}

	nodeLogger := logger.With(
		"nodeID", node.ID,
		"nodeName", node.Name,
	)
	nodeLogger.Info("Deleting surplus no-nodepool control-plane node", "reason", reason)
	started, err := r.teardownOldNode(
		ctx,
		cp,
		node,
		reason,
		"control-plane-scale-down",
		nodeLogger,
	)
	if err != nil {
		return false, fmt.Errorf("tearing down surplus control-plane node %q: %w", node.Name, err)
	}
	return started, nil
}

// scaleDownSurgeNodes removes cluster-level surge control-plane nodes when max surge
// is configured at cluster level rather than nodepool level. Returns true when a surge
// node delete was attempted and the caller should wait before proceeding.
func (r *reconciler) scaleDownSurgeNodes(
	ctx context.Context,
	cluster db.Cluster,
	cp *controlPlane,
	cpNodePools []db.NodePool,
	logger *slog.Logger,
) (bool, error) {
	if !isSurgeAtClusterLevel(cluster, cpNodePools) {
		return false, nil
	}

	managedNodes, err := r.getControlPlaneControllerManagedNodes(ctx, cluster.ID)
	if err != nil {
		return false, err
	}
	// Nothing to scale down unless a surge node is present (count > desired). Under
	// the control-plane-controller path all CP nodes are managed, so compare
	// against desired rather than 0.
	if len(managedNodes) <= cluster.ControlPlaneConfig.DesiredCount {
		return false, nil
	}
	deleted, err := r.deleteNode(ctx, cluster, cp, managedNodes[0], "max-surge scale down", logger)
	if err != nil {
		return deleted, err
	}
	return deleted, nil
}

func generateControlPlaneNodeName(existingNames []string, clusterName, prefix string) string {
	for {
		suffix := strings.Split(uuid.NewString(), "-")[0]
		name := fmt.Sprintf("%s-%s-%s", clusterName, prefix, suffix)
		if !slices.Contains(existingNames, name) {
			return name
		}
	}
}
