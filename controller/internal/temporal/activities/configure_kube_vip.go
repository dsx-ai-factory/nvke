// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nke/controller/internal/clients"
	"github.com/dsx-ai-factory/nke/controller/internal/nodes"
)

const (
	configureKubeVipRunbookName = "configure-kube-vip"
	fmdsBGPPeerAS               = "FROM_FMDS"
)

type ConfigureKubeVipInput struct {
	NodeID    uuid.UUID
	ClusterID uuid.UUID
	// ConfigureSuperAdmin is used only by the mixed-fleet legacy fallback.
	// Typed reconciliation never creates an elevated kubeconfig.
	ConfigureSuperAdmin bool

	NodeProvisioningConditionMessage    string
	ClusterProvisioningConditionMessage string
}

type ConfigureKubeVipResult struct {
	// ConfigureSuperAdminOutput is populated only by the legacy fallback.
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

	// Native-networking sites learn the per-node DPU ASN from FMDS. The
	// site-scoped endpoint supports every infrastructure provider; VPC prefixes
	// remain a compatibility signal for existing NICo deployments.
	const defaultBGPPeerASN uint32 = 4294967000
	bgpPeerAS := "4294967000"
	bgpPeerASN := defaultBGPPeerASN
	bgpPeerASNSource := nkeproto.ReconcileKubeVipRequest_BGP_PEER_ASN_SOURCE_STATIC
	fmdsEndpoint := a.cfg.KubeVipFMDSEndpoint
	if fmdsEndpoint != "" {
		bgpPeerAS = fmdsBGPPeerAS
		bgpPeerASN = 0
		bgpPeerASNSource = nkeproto.ReconcileKubeVipRequest_BGP_PEER_ASN_SOURCE_FMDS
	} else {
		provider, err := clients.GetProviderClient(cluster)
		if err != nil {
			return ConfigureKubeVipResult{}, err
		}
		useFMDS, err := provider.KubeVIPConfig().UsesFMDSBGPPeerASN(nodePool)
		if err != nil {
			return ConfigureKubeVipResult{}, err
		}
		if useFMDS {
			bgpPeerAS = fmdsBGPPeerAS
			bgpPeerASN = 0
			bgpPeerASNSource = nkeproto.ReconcileKubeVipRequest_BGP_PEER_ASN_SOURCE_FMDS
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
	heartbeatCancel := startActivityHeartbeat(ctx)
	defer heartbeatCancel()
	reconcileResponse, err := a.nodeAgentClient.ReconcileKubeVip(ctx, node, &nkeproto.ReconcileKubeVipRequest{
		VipAddress:       cluster.ControlPlaneVIP,
		BgpAsn:           kubeVipASN,
		NodeIp:           node.IP,
		BgpPeerAsnSource: bgpPeerASNSource,
		BgpPeerAsn:       bgpPeerASN,
		FmdsEndpoint:     fmdsEndpoint,
	})
	if err == nil {
		if reconcileResponse == nil {
			return ConfigureKubeVipResult{}, fmt.Errorf("node agent returned an empty kube-vip reconciliation response")
		}
		switch reconcileResponse.GetOutcome() {
		case nkeproto.ReconcileKubeVipResponse_OUTCOME_UNSPECIFIED:
			return ConfigureKubeVipResult{}, fmt.Errorf("node agent returned an unspecified kube-vip reconciliation outcome")
		case nkeproto.ReconcileKubeVipResponse_OUTCOME_CREATED,
			nkeproto.ReconcileKubeVipResponse_OUTCOME_UPDATED,
			nkeproto.ReconcileKubeVipResponse_OUTCOME_UNCHANGED:
			return ConfigureKubeVipResult{
				ConfigureKubeVipOutput: fmt.Sprintf(
					"kube-vip %s: manifest=%s interface=%s peer=%s peerASN=%d kubeconfig=%s kubeconfigChanged=%t cleanedInterfaces=%v",
					reconcileResponse.GetOutcome(),
					reconcileResponse.GetManifestPath(),
					reconcileResponse.GetBgpInterface(),
					reconcileResponse.GetBgpPeerAddress(),
					reconcileResponse.GetBgpPeerAsn(),
					reconcileResponse.GetKubeconfigPath(),
					reconcileResponse.GetKubeconfigChanged(),
					reconcileResponse.GetCleanedInterfaces(),
				),
			}, nil
		default:
			return ConfigureKubeVipResult{}, fmt.Errorf("node agent returned invalid kube-vip reconciliation outcome %s", reconcileResponse.GetOutcome())
		}
	}
	if status.Code(err) != codes.Unimplemented {
		return ConfigureKubeVipResult{}, fmt.Errorf("failed to reconcile kube-vip on node %s: %w", node.Name, err)
	}
	logger.Info("ReconcileKubeVip is unavailable; falling back to legacy kube-vip runbooks",
		"nodeID", node.ID,
		"nodeName", node.Name,
	)

	// Keep elevated kubeconfig generation below the Unimplemented gate. The
	// typed path is deliberately credential-less; super-admin exists only to
	// support agents that still require the legacy runbook.
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
		RunbookName: configureKubeVipRunbookName,
		RunbookEnv: map[string]string{
			"VIP_ADDRESS":   cluster.ControlPlaneVIP,
			"BGP_AS":        fmt.Sprintf("%d", kubeVipASN),
			"BGP_PEER_AS":   bgpPeerAS,
			"FMDS_ENDPOINT": fmdsEndpoint,
			"NODE_IP":       node.IP,
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
