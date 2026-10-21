// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	// The helm value that overrides the registry CNI images are pulled from.
	cniRegistryHelmValue = "global.registry"
)

type InstallCNIInput struct {
	ClusterID     uuid.UUID
	InitialNodeID uuid.UUID

	ClusterProvisioningConditionMessage string
}

// postInitCNIVariant maps a cluster's CNI plugin to the variant the agent expects.
// The second return is false for a plugin with no post-init install path, so an
// unknown plugin refuses rather than silently taking a default.
func postInitCNIVariant(plugin db.CniPlugin) (nvkeproto.CniVariant, bool) {
	switch plugin {
	case db.CNICalico:
		return nvkeproto.CniVariant_CNI_VARIANT_CALICO, true
	case db.CNIOvnKubernetes, db.CNIOvnKubernetesDPUHost:
		return nvkeproto.CniVariant_CNI_VARIANT_OVN, true
	default:
		return nvkeproto.CniVariant_CNI_VARIANT_UNSPECIFIED, false
	}
}

func postInitCNISupported(plugin db.CniPlugin) bool {
	_, ok := postInitCNIVariant(plugin)
	return ok
}

// CNIInstallMode reports whether this cluster gets CNI after kubeadm init
// rather than during it. KubeadmInit and InstallCNI both need the same answer,
// so the workflow asks once and passes it to both.
func (a *Activities) CNIInstallMode(ctx context.Context, clusterID uuid.UUID) (bool, error) {
	if !a.cfg.InstallCNIAfterInit {
		return false, nil
	}
	cluster, err := a.db.GetCluster(ctx, clusterID)
	if err != nil {
		return false, fmt.Errorf("failed to get cluster: %w", err)
	}
	return postInitCNISupported(cluster.CniPlugin), nil
}

// InstallCNI brings up cluster networking on the node that ran kubeadm init.
// The agent reads the manifests from a Debian package already installed on the
// node, so the node needs no bundle server. Other nodes inherit the CNI when
// they join.
func (a *Activities) InstallCNI(ctx context.Context, input InstallCNIInput) error {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[struct{}]{
		Message:         input.ClusterProvisioningConditionMessage,
		UpdateCondition: a.clusterProvisioningConditionMessageUpdater(input.ClusterID),
		RenderError:     messageForTemporalError,
		ActivityBody: func() (struct{}, error) {
			return struct{}{}, a.installCNI(ctx, input)
		},
	}

	_, err := conditionUpdateWrapper.Run(ctx)
	return err
}

func (a *Activities) installCNI(ctx context.Context, input InstallCNIInput) error {
	cluster, err := a.db.GetCluster(ctx, input.ClusterID)
	if err != nil {
		return fmt.Errorf("failed to get cluster: %w", err)
	}
	node, err := a.db.GetNode(ctx, input.InitialNodeID)
	if err != nil {
		return fmt.Errorf("failed to get initial control plane node: %w", err)
	}

	req, err := installCNIRequest(cluster, a.cfg.CNIImageRegistry)
	if err != nil {
		return temporal.NewNonRetryableApplicationError(err.Error(), "UnsupportedCNIPlugin", nil)
	}

	activity.GetLogger(ctx).Info("installing CNI",
		"nodeName", node.Name, "cniPlugin", cluster.CniPlugin, "cniVersion", req.GetVersion())

	// Synchronous: no status to poll afterwards.
	if _, err := a.nodeAgentClient.InstallCNI(ctx, node, req); err != nil {
		return installCNIError(node.Name, err)
	}
	return nil
}

// installCNIError marks failures that cannot succeed on a later attempt as
// non-retryable. This activity retries forever, so without this a bad agent or
// a missing package leaves the cluster stuck in provisioning.
func installCNIError(nodeName string, err error) error {
	switch status.Code(err) { //nolint:exhaustive // the default keeps every other code retryable
	case codes.Unimplemented:
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("node %s runs an agent that does not serve InstallCNI: the post-init CNI path needs nvke-agent 0.2.0 or later", nodeName),
			"AgentTooOld", err,
		)
	// Missing charts package, a worker node, no Open vSwitch on the host.
	case codes.FailedPrecondition, codes.InvalidArgument:
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("failed to install CNI on node %s: %v", nodeName, err),
			"InstallCNIPreconditionFailed", err,
		)
	default:
		return fmt.Errorf("failed to install CNI on node %s: %w", nodeName, err)
	}
}

// postInitCNIVersion is the CNI version to ask the agent for. Empty means
// whatever the agent defaults to, which is what almost every cluster wants.
//
// Do not send a version that matches that default: the agent turns a non-empty
// value into an apt "=" pin as well as a package name, and a deb revision like
// 3.30.1-2 does not match a 3.30.1 pin.
func postInitCNIVersion(cluster db.Cluster) string {
	version := strings.TrimSpace(cluster.CniVersion)
	if cluster.CniPlugin == db.CNIOvnKubernetesDPUHost {
		return db.EffectiveCNIVersion(cluster.CniPlugin, version)
	}
	if defaultVersion, ok := db.DefaultCNIVersion(cluster.CniPlugin); ok && version == defaultVersion {
		return ""
	}
	return version
}

func installCNIRequest(cluster db.Cluster, cniImageRegistry string) (*nvkeproto.InstallCNIRequest, error) {
	variant, ok := postInitCNIVariant(cluster.CniPlugin)
	if !ok {
		return nil, fmt.Errorf("CNI plugin %s has no post-init install path", cluster.CniPlugin)
	}

	// KubeadmInit sets this too, but the agent ignores it when it skips its own
	// CNI install, so repeat it here.
	var cniConfig map[string]string
	if cniImageRegistry != "" {
		cniConfig = map[string]string{cniRegistryHelmValue: cniImageRegistry}
	}
	if cluster.CniPlugin == db.CNIOvnKubernetesDPUHost {
		if cniConfig == nil {
			cniConfig = make(map[string]string)
		}
		cniConfig["nvke.ovnDpuHost"] = "true"
	}

	return &nvkeproto.InstallCNIRequest{
		CniVariant:     variant,
		CniConfig:      cniConfig,
		Version:        postInitCNIVersion(cluster),
		PodNetworkCidr: cluster.PodCIDR,
		ServiceCidr:    cluster.ServiceCIDR,
	}, nil
}
