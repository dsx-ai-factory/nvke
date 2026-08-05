// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
)

type TaintNodeInput struct {
	NodeID uuid.UUID

	TaintKey    string
	TaintValue  string
	TaintEffect string
}

type TaintNodeResult struct {
	Output string
}

func (a *Activities) TaintNode(ctx context.Context, input TaintNodeInput) (TaintNodeResult, error) {
	logger := activity.GetLogger(ctx)

	logger.Info("Tainting node", "nodeID", input.NodeID)

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return TaintNodeResult{}, fmt.Errorf("failed to get node: %w", err)
	}

	cluster, err := a.GetClusterForNode(ctx, node.ID)
	if err != nil {
		return TaintNodeResult{}, fmt.Errorf("failed to get cluster: %w", err)
	}

	result, err := a.ExecuteClusterRunbook(ctx, ExecuteClusterRunbookInput{
		ClusterID:   cluster.ID,
		RunbookName: "taint-kube-node",
		RunbookEnv: map[string]string{
			"NODE_NAMES":   node.Name,
			"TAINT_KEY":    input.TaintKey,
			"TAINT_VALUE":  input.TaintValue,
			"TAINT_EFFECT": input.TaintEffect,
		},
	})
	if err != nil {
		return TaintNodeResult{}, fmt.Errorf("failed to execute cluster runbook: %w", err)
	}

	return TaintNodeResult{
		Output: result.Output,
	}, nil
}
