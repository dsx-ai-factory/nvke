// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nke/controller/internal/temporal/activities"
)

type UpgradeControlPlaneAddonsInput struct {
	ClusterID         uuid.UUID `json:"clusterID"`
	KubernetesVersion string    `json:"kubernetesVersion"`
}

type UpgradeControlPlaneAddonsResult struct {
	// NodeID is the control plane node the kubeadm upgrade ran on.
	NodeID uuid.UUID `json:"nodeID"`
	// NodeName is the name of the control plane node the kubeadm upgrade ran on.
	NodeName string `json:"nodeName"`
	// Output is the stdout from kubeadm plan and apply.
	Output string `json:"output"`
}

// UpgradeControlPlaneAddons runs kubeadm upgrade apply through the node agent on
// a healthy control-plane node to upgrade cluster add-ons (kube-proxy, CoreDNS)
// to the target Kubernetes version, then re-applies NKE DNS overlays via
// InstallKubeLocalDNS and the Pod Security Admission labels via
// LabelSystemNamespacesForPSA. The kubelet is upgraded separately through node
// replacement, and kubeadm upgrade apply is safe to retry.
func UpgradeControlPlaneAddons(ctx workflow.Context, input UpgradeControlPlaneAddonsInput) (UpgradeControlPlaneAddonsResult, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("Upgrading control plane add-ons",
		"clusterID", input.ClusterID,
		"kubernetesVersion", input.KubernetesVersion)

	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		HeartbeatTimeout:    defaultRunbookActivityHeartbeatTimeout(),
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 30 * time.Second,
			MaximumInterval: 2 * time.Minute,
			MaximumAttempts: 3,
		},
	}

	var result activities.KubeadmUpgradeApplyResult
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.KubeadmUpgradeApply,
		activities.KubeadmUpgradeApplyInput{
			ClusterID:         input.ClusterID,
			KubernetesVersion: input.KubernetesVersion,
		},
	).Get(ctx, &result); err != nil {
		return UpgradeControlPlaneAddonsResult{}, fmt.Errorf("failed to upgrade control plane add-ons: %w", err)
	}

	logger.Info("Upgraded control plane add-ons",
		"clusterID", input.ClusterID,
		"kubernetesVersion", input.KubernetesVersion,
		"nodeID", result.NodeID,
		"nodeName", result.NodeName)

	// kubeadm upgrade apply can rewrite CoreDNS / kube-dns; re-apply NKE DNS
	// overlays (node-local-dns, prefer-local annotations, dns-role labels).
	dnsActivityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 30 * time.Second,
			MaximumInterval: 2 * time.Minute,
			// Retry while waiting for a Ready dns-role=cluster-dns pod before
			// rewriting the kube-dns selector (CoreDNS / node-local-dns rollout).
			MaximumAttempts: 8,
		},
	}
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, dnsActivityOptions),
		a.InstallKubeLocalDNS,
		activities.InstallKubeLocalDNSInput{
			ClusterID: input.ClusterID,
		},
	).Get(ctx, nil); err != nil {
		return UpgradeControlPlaneAddonsResult{}, fmt.Errorf("failed to reconcile kube-local DNS after addon upgrade: %w", err)
	}

	// Upgrades started before this step existed must replay without it.
	if workflow.GetVersion(ctx, psaLabelSystemNamespacesChangeID, workflow.DefaultVersion, 1) >= 1 {
		if err := labelSystemNamespacesForPSA(ctx, input.ClusterID, ""); err != nil {
			return UpgradeControlPlaneAddonsResult{}, err
		}
	}

	return UpgradeControlPlaneAddonsResult{
		NodeID:   result.NodeID,
		NodeName: result.NodeName,
		Output:   result.Output,
	}, nil
}
