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
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/NVIDIA/nke/controller/internal/providers/forge"
)

type DeleteForgeInstanceInput struct {
	NodeID             uuid.UUID
	MachineHealthIssue *forge.MachineHealthIssue

	NodeDeletingConditionMessage string
}

type DeleteForgeInstanceResult struct {
	InstanceID   string
	InstanceName string
	InstanceIP   string
}

func (a *Activities) DeleteForgeInstance(ctx context.Context, input DeleteForgeInstanceInput) (DeleteForgeInstanceResult, error) {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[DeleteForgeInstanceResult]{
		Message:         input.NodeDeletingConditionMessage,
		UpdateCondition: a.nodeDeletingConditionMessageUpdater(input.NodeID),
		RenderError:     messageForTemporalError,
		ActivityBody: func() (DeleteForgeInstanceResult, error) {
			return a.deleteForgeInstance(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) deleteForgeInstance(ctx context.Context, input DeleteForgeInstanceInput) (DeleteForgeInstanceResult, error) {
	logger := activity.GetLogger(ctx)

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return DeleteForgeInstanceResult{}, fmt.Errorf("failed to get node: %w", err)
	}

	clusterID := node.ClusterID
	nodePoolID := node.NodePoolID

	cluster, err := a.db.GetCluster(ctx, clusterID)
	if err != nil {
		return DeleteForgeInstanceResult{}, fmt.Errorf("failed to get cluster: %w", err)
	}
	nicoClusterProvider, err := cluster.NicoProvider()
	if err != nil {
		return DeleteForgeInstanceResult{}, err
	}
	nicoNodeProvider, err := node.NicoProvider()
	if err != nil {
		return DeleteForgeInstanceResult{}, err
	}

	if nicoNodeProvider.ForgeInstanceID == "" {
		logger.Info("ForgeInstanceID is empty, skipping forge instance deletion",
			"nodeID", input.NodeID,
			"nodeName", node.Name,
		)
		// No forge instance to delete
		return DeleteForgeInstanceResult{}, nil
	}

	logger.Info("Deleting Forge instance",
		"clusterID", cluster.ID,
		"nodePoolID", nodePoolID,
		"nodeID", input.NodeID,
		"nodeName", node.Name,
		"forgeInstanceID", nicoNodeProvider.ForgeInstanceID,
		"forgeMachineID", nicoNodeProvider.ForgeMachineID,
		"machineHealthIssue", input.MachineHealthIssue,
	)

	// Perform the delete operation
	// If input.MachineHealthIssue is not nil, it signals to Forge to flag the instance for repair.
	// See https://nvmetal.gitlab-master-pages.nvidia.com/carbide/breakfix_integration/breakfix_integration.html#what-happens-when-you-report-issues
	err = a.forgeProvider.DeleteInstance(ctx, nicoClusterProvider.OrgID, nicoNodeProvider.ForgeInstanceID, input.MachineHealthIssue)
	if err != nil {
		// Bad Request - Should not be retried
		if errors.Is(err, forge.ErrBadRequest) {
			return DeleteForgeInstanceResult{}, temporal.NewNonRetryableApplicationError(
				"failed to execute delete Forge instance",
				"forge.BadRequestError",
				fmt.Errorf("failed to execute delete Forge instance: %w", err),
			)
		}

		return DeleteForgeInstanceResult{}, fmt.Errorf("failed to execute delete Forge instance: %w", err)
	}

	logger.Info("Forge instance deleted successfully",
		"clusterID", cluster.ID,
		"nodePoolID", nodePoolID,
		"nodeID", input.NodeID,
		"nodeName", node.Name,
		"forgeInstanceID", nicoNodeProvider.ForgeInstanceID,
		"forgeMachineID", nicoNodeProvider.ForgeMachineID,
	)

	return DeleteForgeInstanceResult{
		InstanceID:   nicoNodeProvider.ForgeInstanceID,
		InstanceName: node.Name,
		InstanceIP:   node.IP,
	}, nil
}
