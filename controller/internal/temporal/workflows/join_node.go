// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/workflow"

	"github.com/NVIDIA/nke/controller/internal/metrics"
)

// JoinNodeInput starts kubeadm join and post-join for a host that is already ReadyToJoin.
type JoinNodeInput struct {
	NodeID uuid.UUID
}

// JoinNode joins a node that has already completed host provisioning. Worker
// nodes typically run this inline from ProvisionNode; control-plane nodes are
// joined by the control-plane controller, one at a time.
func JoinNode(ctx workflow.Context, input JoinNodeInput) error {
	logger := workflow.GetLogger(ctx)

	if input.NodeID == uuid.Nil {
		return errors.New("nodeID is required")
	}

	workflowStart := workflow.Now(ctx)

	labels, labelsErr := getNodeMetricLabels(ctx, input.NodeID)
	if labelsErr != nil {
		logger.Warn("failed to get node metric labels, metrics will use unknown", "error", labelsErr)
		labels = nodeMetricLabels{ClusterName: unknownValue, NodeType: unknownValue}
	}

	nodeCtx, err := getNodeProvisioningContext(ctx, input.NodeID)
	if err != nil {
		return fmt.Errorf("failed to get node provisioning context: %w", err)
	}

	// Bound the join to a single attempt so a failure fails this workflow run;
	// the workflow-level RetryPolicy set by the control-plane controller owns the
	// retry budget (retry X times, then rotate to the next control-plane node).
	if err := joinProvisionedNode(ctx, input.NodeID, nodeCtx, labels, true); err != nil {
		// Best effort to update the node conditions to indicate the join failed
		if err := updateNodeProvisioningMessage(ctx, input.NodeID, fmt.Sprintf("Workflow failed: %s", err)); err != nil {
			logger.Error("failed to update node conditions", "error", err)
		}
		if !workflow.IsReplaying(ctx) {
			metrics.RecordProvisionFailed(labels.ClusterName, labels.NodeType, workflow.Now(ctx).Sub(workflowStart))
		}
		return fmt.Errorf("failed to join node: %w", err)
	}

	// Report provision-completed end-to-end to match the inline worker path
	// (ProvisionNode), which measures from the start of host provisioning. This
	// workflow starts only after the host is ReadyToJoin, so anchor on the node's
	// creation time instead of workflowStart. Best-effort: fall back to the
	// join-scoped duration if the node read fails, so a metric never fails an
	// otherwise-successful join.
	totalDuration := workflow.Now(ctx).Sub(workflowStart)
	if provisionStart, err := nodeProvisioningStart(ctx, input.NodeID); err != nil {
		logger.Warn("failed to read node creation time; provision-completed metric covers the join only",
			"nodeID", input.NodeID, "error", err)
	} else {
		totalDuration = workflow.Now(ctx).Sub(provisionStart)
	}
	if !workflow.IsReplaying(ctx) {
		metrics.RecordProvisionCompleted(labels.ClusterName, labels.NodeType, totalDuration)
	}

	logger.Info("Node join complete",
		"nodeID", input.NodeID,
		"duration", totalDuration.Seconds(),
		"clusterName", labels.ClusterName,
		"nodeType", labels.NodeType,
	)

	return nil
}
