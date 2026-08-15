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

	"github.com/NVIDIA/nke/controller/internal/nodes"
)

type ConfigureKubeVipInput struct {
	NodeID              uuid.UUID
	ClusterID           uuid.UUID
	ConfigureSuperAdmin bool

	NodeProvisioningConditionMessage    string
	ClusterProvisioningConditionMessage string
}

type ConfigureKubeVipResult struct {
	ConfigureSuperAdminOutput *string
	ConfigureKubeVipOutput    string
}

func (a *Activities) ConfigureKubeVip(ctx context.Context, input ConfigureKubeVipInput) (ConfigureKubeVipResult, error) {
	var conditionMessage string
	var updateCondition func(ctx context.Context, message string) error

	if input.NodeProvisioningConditionMessage != "" {
		conditionMessage = input.NodeProvisioningConditionMessage
		updateCondition = a.nodeProvisioningConditionMessageUpdater(input.NodeID)
	} else if input.ClusterProvisioningConditionMessage != "" {
		conditionMessage = input.ClusterProvisioningConditionMessage
		updateCondition = a.clusterProvisioningConditionMessageUpdater(input.ClusterID)
	}

	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[ConfigureKubeVipResult]{
		Message:         conditionMessage,
		UpdateCondition: updateCondition,
		RenderError:     messageForTemporalError,
		ActivityBody: func() (ConfigureKubeVipResult, error) {
			return a.configureKubeVip(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) configureKubeVip(ctx context.Context, input ConfigureKubeVipInput) (ConfigureKubeVipResult, error) {
	logger := activity.GetLogger(ctx)

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return ConfigureKubeVipResult{}, fmt.Errorf("failed to get node: %w", err)
	}

	_, nodePool, err := nodes.GetNodeContext(ctx, a.db, node)
	if err != nil {
		return ConfigureKubeVipResult{}, fmt.Errorf("failed to get node context: %w", err)
	}

	cluster, err := a.GetClusterForNode(ctx, node.ID)
	if err != nil {
		return ConfigureKubeVipResult{}, fmt.Errorf("failed to get cluster: %w", err)
	}

	if !nodePool.ControlPlane {
		// Not a control plane node, so we don't need to configure KubeVip
		return ConfigureKubeVipResult{}, nil
	}

	if cluster.ControlPlaneVIP == "" {
		// No control plane VIP, so we don't need to configure KubeVip
		return ConfigureKubeVipResult{}, nil
	}

	const kubeVipASN = 65100

	// When FNN (Forge Native Networking) is enabled in the site we want to query the DPU ASN from the Forge Metadata Service (FMDS).
	// We know FNN is enabled when the node pool has VPC prefixes instead of subnets.
	// CAPI clusters do not use Nico provider payloads or VPC prefixes, so skip the check for them.
	bgpPeerAS := "4294967000"
	if nodePool.Nico != nil {
		nicoNodePoolProvider, err := nodePool.NicoProvider()
		if err != nil {
			return ConfigureKubeVipResult{}, err
		}
		if len(nicoNodePoolProvider.VpcPrefixIDs) > 0 {
			bgpPeerAS = "FROM_FMDS"
		}
	}

	logger.Info("Configuring kube-vip on control plane node",
		"clusterID", cluster.ID,
		"controlPlaneVIP", cluster.ControlPlaneVIP,
		"nodeID", node.ID,
		"nodeIP", node.IP,
		"kubeVipASN", kubeVipASN,
		"bgpPeerAS", bgpPeerAS,
	)

	// Assume host ID is the same as node ID by convention
	hostID := node.ID

	var configureSuperAdminOutput *string
	if input.ConfigureSuperAdmin {
		configureSuperAdminResult, err := a.ExecuteHostRunbook(ctx, ExecuteHostRunbookInput{
			HostID:      hostID,
			RunbookName: "configure-super-admin",
			RunbookEnv:  map[string]string{},
		})
		if err != nil {
			return ConfigureKubeVipResult{}, fmt.Errorf("failed to configure super-admin kubeconfig: %w", err)
		}
		configureSuperAdminOutput = &configureSuperAdminResult.Output
	}

	configureKubeVipResult, err := a.ExecuteHostRunbook(ctx, ExecuteHostRunbookInput{
		HostID:      hostID,
		RunbookName: "configure-kube-vip",
		RunbookEnv: map[string]string{
			"VIP_ADDRESS": cluster.ControlPlaneVIP,
			"BGP_AS":      fmt.Sprintf("%d", kubeVipASN),
			"BGP_PEER_AS": bgpPeerAS,
			"NODE_IP":     node.IP,
		},
	})
	if err != nil {
		return ConfigureKubeVipResult{}, fmt.Errorf("failed to configure kube-vip: %w", err)
	}

	return ConfigureKubeVipResult{
		ConfigureSuperAdminOutput: configureSuperAdminOutput,
		ConfigureKubeVipOutput:    configureKubeVipResult.Output,
	}, nil
}
