// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
)

type ExecuteClusterRunbookHookInput struct {
	ClusterID   uuid.UUID
	HookPhase   string
	RunbookName string
	RunbookEnv  map[string]string

	ClusterProvisioningConditionMessage string
}

type ExecuteClusterRunbookHookResult struct {
	Output string
}

// ExecuteClusterRunbookHook executes a cluster runbook and updates the cluster Provisioning condition.
func (a *Activities) ExecuteClusterRunbookHook(
	ctx context.Context,
	input ExecuteClusterRunbookHookInput,
) (ExecuteClusterRunbookHookResult, error) {
	logger := activity.GetLogger(ctx)

	logger.Info("Executing runbook hook", "clusterID", input.ClusterID, "hookPhase", input.HookPhase, "runbookName", input.RunbookName)

	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[ExecuteClusterRunbookHookResult]{
		Message:         input.ClusterProvisioningConditionMessage,
		UpdateCondition: a.clusterProvisioningConditionMessageUpdater(input.ClusterID),
		RenderError:     messageForTemporalError,
		ActivityBody: func() (ExecuteClusterRunbookHookResult, error) {
			result, err := a.ExecuteClusterRunbook(ctx, ExecuteClusterRunbookInput{
				ClusterID:   input.ClusterID,
				RunbookName: input.RunbookName,
				RunbookEnv:  input.RunbookEnv,
			})

			logger.Info("Runbook hook executed successfully", "clusterID", input.ClusterID, "hookPhase", input.HookPhase, "name", input.RunbookName)

			return ExecuteClusterRunbookHookResult{
				Output: result.Output,
			}, err
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}
