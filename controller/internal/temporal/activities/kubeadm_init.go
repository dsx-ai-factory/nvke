// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/kubeadmconfig"
	"github.com/NVIDIA/nke/controller/internal/nodes"
)

type KubeadmInitInput struct {
	ClusterID     uuid.UUID
	InitialNodeID uuid.UUID

	ClusterProvisioningConditionMessage string
}

func (a *Activities) KubeadmInit(ctx context.Context, input KubeadmInitInput) error {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[struct{}]{
		Message:         input.ClusterProvisioningConditionMessage,
		UpdateCondition: a.clusterProvisioningConditionMessageUpdater(input.ClusterID),
		RenderError:     messageForTemporalError,
		ActivityBody: func() (struct{}, error) {
			return struct{}{}, a.kubeadmInit(ctx, input)
		},
	}

	_, err := conditionUpdateWrapper.Run(ctx)
	return err
}

func (a *Activities) kubeadmInit(ctx context.Context, input KubeadmInitInput) error {
	logger := activity.GetLogger(ctx)

	cluster, err := a.db.GetCluster(ctx, input.ClusterID)
	if err != nil {
		return fmt.Errorf("failed to get cluster: %w", err)
	}
	node, err := a.db.GetNode(ctx, input.InitialNodeID)
	if err != nil {
		return fmt.Errorf("failed to get initial control plane node: %w", err)
	}

	_, nodePool, err := nodes.GetNodeContext(ctx, a.db, node)
	if err != nil {
		return fmt.Errorf("failed to get node context for initial node: %w", err)
	}

	kubeadmContainerMode, err := a.kubeadmContainerMode(ctx, cluster, node)
	if err != nil {
		return err
	}

	// Use the control plane VIP if set, otherwise fallback to the node IP
	controlPlaneEndpoint := cluster.ControlPlaneVIP
	if controlPlaneEndpoint == "" {
		controlPlaneEndpoint = node.IP
	}

	status, err := a.nodeAgentClient.GetKubeadmStatus(ctx, node)
	if err != nil {
		return fmt.Errorf("failed to get kubeadm status: %w", err)
	}
	kubeadmPhase := strings.ToLower(status.GetKubeadmPhase())
	kubeadmStatus := strings.ToLower(status.GetKubeadmStatus())

	kubeadmInitAlreadyStarted := kubeadmPhase == kubeadmPhaseInit && (kubeadmStatus == kubeadmStatusReady || kubeadmStatus == kubeadmStatusPending)

	if kubeadmInitAlreadyStarted {
		logger.Info("node already initialized or initializing", "nodeID", node.ID, "nodeName", node.Name)
		if cluster.EtcdEncrypted {
			if _, err := a.requireKubeAPIEncryptionKey(ctx, cluster.ID); err != nil {
				return err
			}
		}
		return a.waitForKubeadmInitReady(ctx, node)
	}

	initConfig := &kubeadmconfig.InitConfig{
		NodeIP:                node.IP,
		APIServerEndpoint:     controlPlaneEndpoint,
		NodeName:              node.Name,
		KubernetesVersion:     cluster.KubernetesVersion,
		PodSubnet:             cluster.PodCIDR,
		ServiceSubnet:         cluster.ServiceCIDR,
		EtcdQuotaBackendBytes: kubeadmconfig.EtcdQuotaBackendBytes,
		Labels:                nodeLabels(nodePool.NodeLabels).String(),
		ImageRepository:       a.cfg.KubeadmImageRepository,
		SkipKubeProxy:         cluster.CniPlugin == db.CNIOvnKubernetes,
		EnableEncryption:      cluster.EtcdEncrypted,
		KubeadmContainerMode:  kubeadmContainerMode,
		SystemReservedMemory:  nodePool.SystemReservedMemory,
		KubeReservedMemory:    nodePool.KubeReservedMemory,
	}
	if cluster.OIDCAuthEnabled {
		initConfig.AuthenticationConfig, err = a.buildOIDCAuthenticationConfig(ctx, cluster.Name)
		if err != nil {
			return fmt.Errorf("failed to build OIDC authentication config: %w", err)
		}
	}

	artifacts, err := initConfig.Render()
	if err != nil {
		return fmt.Errorf("failed to render kubeadm init config: %w", err)
	}

	req := &nkeproto.KubeadmInitRequest{
		InitConfig:           string(artifacts.ConfigYAML),
		ControlPlaneEndpoint: controlPlaneEndpoint,
		PodNetworkCidr:       cluster.PodCIDR,
		ServiceCidr:          cluster.ServiceCIDR,
		ApiEncryptionEnabled: cluster.EtcdEncrypted,
		ObjectsToEncrypt:     []string{"secrets"},
		Files:                toNodeAgentFiles(artifacts.Files),
	}
	if cluster.EtcdEncrypted {
		req.ApiEncryptionKey, err = a.ensureKubeAPIEncryptionKey(ctx, cluster.ID)
		if err != nil {
			return fmt.Errorf("failed to ensure kube-api encryption key: %w", err)
		}
	}
	if a.cfg.CNIImageRegistry != "" {
		req.CniConfig = map[string]string{"global.registry": a.cfg.CNIImageRegistry} //nolint:staticcheck // Deprecated in-init CNI path still consumes CniConfig for registry overrides.
	}

	// Use the cluster's configured CNI version (falling back to the plugin default when unset) so the
	// in-init CNI manifests match the CNI image package pinned in the node's user-data. See
	// db.EffectiveCNIVersion and getUserData's CNIImagePackage.
	cniVersion := db.EffectiveCNIVersion(cluster.CniPlugin, strings.TrimSpace(cluster.CniVersion))
	switch cluster.CniPlugin {
	case db.CNIOvnKubernetes:
		req.UseOvnK8SCni = true     //nolint:staticcheck // Deprecated agent in-init CNI path still consumes legacy fields.
		req.CniPluginName = "ovn"   //nolint:staticcheck // Legacy in-init CNI install still switches on this deprecated agent field.
		req.OvnVersion = cniVersion //nolint:staticcheck // Legacy in-init CNI install still consumes deprecated version fields.
	case db.CNICalico:
		req.CniPluginName = "calico"   //nolint:staticcheck // Legacy in-init CNI install still switches on this deprecated agent field.
		req.TigeraVersion = cniVersion //nolint:staticcheck // Legacy in-init CNI install still consumes deprecated version fields.
	default:
		return fmt.Errorf("unsupported CNI plugin: %s", cluster.CniPlugin)
	}

	if _, err := a.nodeAgentClient.ExecuteKubeadmInit(ctx, node, req); err != nil {
		return fmt.Errorf("failed to execute kubeadm init on node %s: %w", node.Name, err)
	}

	return a.waitForKubeadmInitReady(ctx, node)
}

func (a *Activities) waitForKubeadmInitReady(ctx context.Context, node db.Node) error {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		status, err := a.nodeAgentClient.GetKubeadmStatus(ctx, node)
		if err != nil {
			return fmt.Errorf("failed to get kubeadm status: %w", err)
		}
		kubeadmStatus := strings.ToLower(status.GetKubeadmStatus())
		kubeadmPhase := strings.ToLower(status.GetKubeadmPhase())
		cniStatus := strings.ToLower(status.GetCniInstallStatus())

		if kubeadmStatus == kubeadmStatusError {
			return fmt.Errorf("kubeadm init failed: %s", status.GetKubeadmCurrentMessage())
		}

		if cniStatus == cniStatusError {
			return fmt.Errorf("CNI install failed: %s", status.GetKubeadmCurrentMessage())
		}

		if kubeadmStatus == kubeadmStatusReady && kubeadmPhase == kubeadmPhaseInit && cniStatus == cniStatusReady {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
