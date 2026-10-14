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

func generateControlPlaneNodeName(existingNames []string, clusterName, prefix string) string {
	for {
		suffix := strings.Split(uuid.NewString(), "-")[0]
		name := fmt.Sprintf("%s-%s-%s", clusterName, prefix, suffix)
		if !slices.Contains(existingNames, name) {
			return name
		}
	}
}
