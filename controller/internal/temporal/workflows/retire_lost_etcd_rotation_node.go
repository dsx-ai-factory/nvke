// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

const (
	retiredEtcdRotationNodeReason = "Lost control-plane node retired from a failed etcd key rotation"
)

// RetireLostEtcdRotationNodeInput identifies the failed rotation owner and the lost node.
type RetireLostEtcdRotationNodeInput struct {
	ClusterID          uuid.UUID `json:"clusterID"`
	RotationWorkflowID string    `json:"rotationWorkflowID"`
	NodeID             uuid.UUID `json:"nodeID"`
}

// RetireLostEtcdRotationNode is an operator-started workflow. It records the
// audited retirement under the retained rotation lock, then tears down only that
// node so the owner's reset can converge on the surviving control plane.
func RetireLostEtcdRotationNode(ctx workflow.Context, input RetireLostEtcdRotationNodeInput) error {
	if input.ClusterID == uuid.Nil || input.NodeID == uuid.Nil || input.RotationWorkflowID == "" {
		return errors.New("clusterID, rotationWorkflowID and nodeID are required")
	}
	var a *activities.Activities
	var result activities.RetireLostEtcdRotationNodeResult
	if err := workflow.ExecuteActivity(withEtcdEncryptionOptions(ctx), a.RetireLostEtcdRotationNode, activities.RetireLostEtcdRotationNodeInput{
		ClusterID: input.ClusterID, RotationWorkflowID: input.RotationWorkflowID, NodeID: input.NodeID,
	}).Get(ctx, &result); err != nil {
		return fmt.Errorf("failed to retire lost etcd rotation node: %w", err)
	}
	if !result.TeardownRequired {
		return nil
	}
	// The usual teardown ID lets the control-plane reconciler restart a failed run
	// once the rotation completes; a repeat of this workflow restarts it earlier.
	childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID:            constants.TeardownNodeWorkflowPrefix + input.NodeID.String(),
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
	})
	if err := workflow.ExecuteChildWorkflow(childCtx, TeardownNode, TeardownNodeInput{
		NodeID: input.NodeID, AllowCPNode: true, ForceUnreachable: true, Reason: retiredEtcdRotationNodeReason,
	}).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to tear down retired etcd rotation node: %w", err)
	}
	return nil
}
