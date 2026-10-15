// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

const (
	RenewK8sCertsWorkflowType = "RenewK8sCerts"
)

type RenewK8sCertsInput struct {
	ClusterID     uuid.UUID `json:"clusterID"`
	ThresholdDays int       `json:"thresholdDays"`
}

type RenewK8sCertsNodeResult struct {
	NodeID       uuid.UUID
	NodeName     string
	Output       string
	ErrorMessage string
	ExitCode     int
}

type RenewK8sCertsResult struct {
	Results []RenewK8sCertsNodeResult
}

func RenewK8sCerts(ctx workflow.Context, input RenewK8sCertsInput) (RenewK8sCertsResult, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("Renewing Kubernetes certs", "clusterID", input.ClusterID, "thresholdDays", input.ThresholdDays)

	var a *activities.Activities

	var controlPlaneNodes []db.Node
	if err := workflow.ExecuteActivity(
		withDBOptions(ctx),
		a.ListControlPlaneNodes,
		input.ClusterID,
	).Get(ctx, &controlPlaneNodes); err != nil {
		return RenewK8sCertsResult{}, fmt.Errorf("failed to list control plane nodes: %w", err)
	}
	if len(controlPlaneNodes) == 0 {
		logger.Info("No control plane nodes found, skipping cert renewal", "clusterID", input.ClusterID)
		return RenewK8sCertsResult{}, nil
	}

	results := make([]RenewK8sCertsNodeResult, 0, len(controlPlaneNodes))

	// Renew one control-plane node at a time. The renewal transaction restarts static
	// control-plane pods, including etcd, so parallel renewal could risk quorum
	// loss or API disruption.
	for _, node := range controlPlaneNodes {
		var result activities.KubernetesCertificateRenewalResult
		workflowInfo := workflow.GetInfo(ctx)
		// Activity retries reuse this ID and therefore reattach to the accepted
		// agent operation; RunID keeps a later workflow run from aliasing it.
		operationID := fmt.Sprintf(
			"k8s-cert-renewal/%s/%s/%s",
			workflowInfo.WorkflowExecution.ID,
			workflowInfo.WorkflowExecution.RunID,
			node.ID,
		)
		if err := workflow.ExecuteActivity(
			workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
				StartToCloseTimeout: 30 * time.Minute,
				HeartbeatTimeout:    defaultRunbookActivityHeartbeatTimeout(),
				RetryPolicy: &temporal.RetryPolicy{
					InitialInterval: 30 * time.Second,
					MaximumInterval: 2 * time.Minute,
					MaximumAttempts: 3,
				},
			}),
			a.RenewKubernetesCertificates,
			activities.KubernetesCertificateRenewalInput{
				NodeID:        node.ID,
				OperationID:   operationID,
				ThresholdDays: input.ThresholdDays,
			},
		).Get(ctx, &result); err != nil {
			return RenewK8sCertsResult{}, fmt.Errorf("failed to renew certs on node %s: %w", node.ID, err)
		}
		results = append(results, RenewK8sCertsNodeResult{
			NodeID:       node.ID,
			NodeName:     node.Name,
			Output:       result.Output,
			ErrorMessage: result.ErrorMessage,
			ExitCode:     result.ExitCode,
		})
	}

	return RenewK8sCertsResult{Results: results}, nil
}
