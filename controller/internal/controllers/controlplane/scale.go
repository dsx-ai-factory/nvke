// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
)

const (
	defaultControlPlaneNodePrefix = "cp"
)

// pendingNodeOption mutates a pending control-plane node before it is created.
// Scale-up and rollout surges pass none; the rebalance step passes
// withRebalanceReplacement so its surge is marked and pinned to a capped config.
type pendingNodeOption func(*db.Node)

// withRebalanceReplacement marks a pending node as a rebalance surge and pins it
// to a preference-capped control-plane-config snapshot. The marker holds the
// node out of scale-down until it joins; the pinned snapshot bounds how far the
// provider's instance-type walk may fall back from the primary type. Both are
// self-resolving: the rebalance step clears the marker and re-pins the node to
// the cluster's current config once it joins.
func withRebalanceReplacement(configurationID uuid.UUID) pendingNodeOption {
	return func(n *db.Node) {
		n.ControlPlaneConfigurationID = configurationID
		n.Conditions.ControlPlanePreferredReplacement = db.NewCondition(true,
			"Rebalance surge: moving the control plane to a more-preferred instance type")
	}
}

// scaleUp creates pending control-plane nodes and returns the number created.
// An allocator seeded with eligible domains spreads new nodes across them.
// Instance-type selection remains the control plane node scheduler's
// responsibility.
func (r *reconciler) scaleUp(
	ctx context.Context,
	cluster db.Cluster,
	count int,
	reason string,
	allocator domainAllocator,
	newNodeProvider clients.NewNodeProvider,
	opts ...pendingNodeOption,
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
		createdNode, err := r.createPendingNode(ctx, cluster, existingNodeNames, failureDomain, newNodeProvider, opts...)
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
	opts ...pendingNodeOption,
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

	// Options run last so a rebalance surge's marker and capped-config pin win
	// over the defaults set above.
	for _, opt := range opts {
		opt(&pendingNode)
	}

	createdNode, err := r.db.CreateNode(ctx, &pendingNode)
	if err != nil {
		return db.Node{}, fmt.Errorf("creating pending control-plane node: %w", err)
	}
	return createdNode, nil
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
