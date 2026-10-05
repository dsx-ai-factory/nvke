// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/kubeadmconfig"
	"github.com/dsx-ai-factory/nke/controller/internal/nodes"
)

type KubeadmInitInput struct {
	ClusterID     uuid.UUID
	InitialNodeID uuid.UUID

	// When true the agent skips its own CNI install and the InstallCNI activity
	// does it instead. See Activities.CNIInstallMode.
	InstallCNIAfterInit bool

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
		return a.waitForKubeadmInitReady(ctx, node, input.InstallCNIAfterInit)
	}

	initConfig := &kubeadmconfig.InitConfig{
		NodeIP:                node.IP,
		APIServerEndpoint:     controlPlaneEndpoint,
		NodeName:              node.Name,
		KubernetesVersion:     cluster.KubernetesVersion,
		PodSubnet:             cluster.PodCIDR,
		ServiceSubnet:         cluster.ServiceCIDR,
		EtcdQuotaBackendBytes: kubeadmconfig.EtcdQuotaBackendBytes,
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
	// The deprecated CNI fields set below are still populated either way; the
	// agent just ignores them when this is true.
	req.NoCniInstall = input.InstallCNIAfterInit

	if a.cfg.CNIImageRegistry != "" {
		req.CniConfig = map[string]string{cniRegistryHelmValue: a.cfg.CNIImageRegistry} //nolint:staticcheck // Deprecated in-init CNI path still consumes CniConfig for registry overrides.
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

	if err := a.ensureKubeadmFiles(ctx, node, artifacts.Files); err != nil {
		return fmt.Errorf("failed to prepare kubeadm files on node %s: %w", node.Name, err)
	}

	if _, err := a.nodeAgentClient.ExecuteKubeadmInit(ctx, node, req); err != nil {
		return fmt.Errorf("failed to execute kubeadm init on node %s: %w", node.Name, err)
	}

	return a.waitForKubeadmInitReady(ctx, node, input.InstallCNIAfterInit)
}

func (a *Activities) waitForKubeadmInitReady(ctx context.Context, node db.Node, installCNIAfterInit bool) error {
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

		// The agent installed no CNI, so it has no CNI status to report. Ignore
		// any left behind by an earlier InstallCNI attempt.
		if installCNIAfterInit {
			cniStatus = cniStatusReady
		}

		// Both are terminal. A retry re-enters kubeadmInitAlreadyStarted, which
		// short-circuits straight back to this poll loop and reads the same
		// status, so ExecuteKubeadmInit is never re-issued. With
		// MaximumAttempts: 0 on the caller that repeats until someone notices.
		if kubeadmStatus == kubeadmStatusError {
			return temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("kubeadm init failed: %s", status.GetKubeadmCurrentMessage()),
				"KubeadmInitFailed", nil,
			)
		}

		if cniStatus == cniStatusError {
			return temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("CNI install failed: %s", status.GetKubeadmCurrentMessage()),
				"CNIInstallFailed", nil,
			)
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
