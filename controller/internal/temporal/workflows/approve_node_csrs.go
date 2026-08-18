// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

const (
	runbookApproveValidatedNodeCSRs = "approve-validated-node-csrs"
)

type ApproveNodeCSRsInput struct {
	ClusterID uuid.UUID `json:"clusterID"`
}

func ApproveNodeCSRs(ctx workflow.Context, input ApproveNodeCSRsInput) (activities.ExecuteClusterRunbookResult, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("Approving node CSRs", "clusterID", input.ClusterID)

	var a *activities.Activities

	var nodes []db.Node
	if err := workflow.ExecuteActivity(
		withDBOptions(ctx),
		a.ListNodes,
		db.NodeFilters{ClusterID: &input.ClusterID},
	).Get(ctx, &nodes); err != nil {
		return activities.ExecuteClusterRunbookResult{}, fmt.Errorf("failed to list cluster nodes: %w", err)
	}

	knownNodes := buildKnownNodes(nodes)
	if knownNodes == "" {
		logger.Info("No nodes with known names and IPs, skipping CSR approval", "clusterID", input.ClusterID)
		return activities.ExecuteClusterRunbookResult{}, nil
	}

	var result activities.ExecuteClusterRunbookResult
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout:    2 * time.Minute,
			ScheduleToCloseTimeout: 7 * time.Minute,
			HeartbeatTimeout:       defaultRunbookActivityHeartbeatTimeout(),
			RetryPolicy: &temporal.RetryPolicy{
				InitialInterval: 2 * time.Second,
				MaximumInterval: 10 * time.Second,
			},
		}),
		a.ExecuteClusterRunbook,
		activities.ExecuteClusterRunbookInput{
			ClusterID:   input.ClusterID,
			RunbookName: runbookApproveValidatedNodeCSRs,
			RunbookEnv: map[string]string{
				"KNOWN_NODES": knownNodes,
			},
		},
	).Get(ctx, &result); err != nil {
		return activities.ExecuteClusterRunbookResult{}, fmt.Errorf("failed to approve node CSRs: %w", err)
	}

	return result, nil
}

func buildKnownNodes(nodes []db.Node) string {
	pairs := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node.Name == "" || node.IP == "" {
			continue
		}
		pairs = append(pairs, fmt.Sprintf("%s=%s", node.Name, node.IP))
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ";")
}
