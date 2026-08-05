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

	forgesdk "gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/nodes"
)

type WaitForForgeInstanceReadyInput struct {
	NodeID uuid.UUID

	NodeProvisioningConditionMessage string
}

func (a *Activities) WaitForForgeInstanceReady(
	ctx context.Context,
	input WaitForForgeInstanceReadyInput,
) (*forgesdk.Instance, error) {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[*forgesdk.Instance]{
		Message:         input.NodeProvisioningConditionMessage,
		UpdateCondition: a.nodeProvisioningConditionMessageUpdater(input.NodeID),
		RenderError:     messageForTemporalError,
		ActivityBody: func() (*forgesdk.Instance, error) {
			return a.waitForForgeInstanceReady(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) waitForForgeInstanceReady(
	ctx context.Context,
	input WaitForForgeInstanceReadyInput,
) (*forgesdk.Instance, error) {
	logger := activity.GetLogger(ctx)

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return nil, fmt.Errorf("failed to get node: %w", err)
	}

	if node.ForgeInstanceID == "" {
		return nil, fmt.Errorf("node %s has no Forge instance ID", input.NodeID)
	}

	_, nodePool, err := nodes.GetNodeContext(ctx, a.db, node)
	if err != nil {
		return nil, fmt.Errorf("failed to get node context: %w", err)
	}

	cluster, err := a.GetClusterForNode(ctx, input.NodeID)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster: %w", err)
	}

	logger.Info("Waiting for Forge instance to be ready",
		"clusterID", cluster.ID,
		"nodePoolID", nodePool.ID,
		"nodeID", input.NodeID,
		"nodeName", node.Name,
		"forgeMachineID", node.ForgeMachineID,
	)

	cachedInstance, err := a.forgeProvider.GetInstanceByID(ctx, cluster.OrgID, cluster.SiteID.String(), node.ForgeInstanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to get cached instance: %w", err)
	}

	var ipAddresses []string
	interfaces := cachedInstance.GetInterfaces()

	if len(interfaces) > 0 {
		ipAddresses = interfaces[0].GetIpAddresses()
	}

	if len(ipAddresses) == 0 {
		return nil, fmt.Errorf("forge instance %s has no IP addresses", cachedInstance.GetId())
	}

	// assume the first ip address is the primary
	ipAddress := ipAddresses[0]

	logger.Info("Forge instance ready",
		"clusterID", cluster.ID,
		"nodePoolID", nodePool.ID,
		"nodeID", input.NodeID,
		"nodeName", node.Name,
		"forgeMachineID", node.ForgeMachineID,
		"ip", ipAddress,
	)

	if err := a.addLabelToInstance(ctx, cluster.OrgID, node.ForgeInstanceID, ipAddress, input.NodeID); err != nil {
		// opportunistic labeling, log error but continue
		logger.Error("Failed to add ip address label to Forge instance", "error", err)
	}

	return cachedInstance, nil
}

func (a *Activities) addLabelToInstance(ctx context.Context, orgID, forgeInstanceID, ipAddress string, nodeID uuid.UUID) error {
	logger := activity.GetLogger(ctx)

	logger.Info("Adding ip address label to Forge instance",
		"forgeInstanceID", forgeInstanceID,
		"ipAddress", ipAddress,
		"nodeID", nodeID,
	)

	_, err := a.forgeProvider.ApplyInstanceLabels(ctx, orgID, forgeInstanceID, map[string]string{
		constants.LabelKeyIPAddress: ipAddress,
	})

	return err
}
