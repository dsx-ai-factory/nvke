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

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
)

type ExecuteNodeRunbookHookInput struct {
	NodeID      uuid.UUID
	HookPhase   string
	RunbookName string
	RunbookEnv  map[string]string

	NodeProvisioningConditionMessage string
}

type ExecuteNodeRunbookHookResult struct {
	Output string
}

// ExecuteNodeRunbookHook executes a node runbook and updates the node Provisioning condition.
func (a *Activities) ExecuteNodeRunbookHook(ctx context.Context, input ExecuteNodeRunbookHookInput) (ExecuteNodeRunbookHookResult, error) {
	logger := activity.GetLogger(ctx)

	logger.Info("Executing runbook hook", "nodeID", input.NodeID, "hookPhase", input.HookPhase, "runbookName", input.RunbookName)

	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[ExecuteNodeRunbookHookResult]{
		Message:         input.NodeProvisioningConditionMessage,
		UpdateCondition: a.nodeProvisioningConditionMessageUpdater(input.NodeID),
		RenderError:     messageForTemporalError,
		ActivityBody: func() (ExecuteNodeRunbookHookResult, error) {
			result, err := a.ExecuteHostRunbook(ctx, ExecuteHostRunbookInput{
				HostID:      input.NodeID, // Same as node ID by convention
				RunbookName: input.RunbookName,
				RunbookEnv:  input.RunbookEnv,
			})

			logger.Info("Runbook hook executed successfully", "nodeID", input.NodeID, "hookPhase", input.HookPhase, "runbookName", input.RunbookName)

			return ExecuteNodeRunbookHookResult{Output: result.Output}, err
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}
