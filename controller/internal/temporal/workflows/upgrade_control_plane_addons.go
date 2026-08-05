// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package workflows

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

type UpgradeControlPlaneAddonsInput struct {
	ClusterID         uuid.UUID `json:"clusterID"`
	KubernetesVersion string    `json:"kubernetesVersion"`
}

type UpgradeControlPlaneAddonsResult struct {
	// NodeID is the control plane node the runbook ran on.
	NodeID uuid.UUID `json:"nodeID"`
	// NodeName is the name of the control plane node the runbook ran on.
	NodeName string `json:"nodeName"`
	// Output is the stdout from the runbook execution.
	Output string `json:"output"`
}

// UpgradeControlPlaneAddons runs the embedded upgrade-kubelet-kubeadm runbook on a
// healthy control plane node via node-agent ExecuteScript to upgrade cluster add-ons
// (kube-proxy, CoreDNS) to the target Kubernetes version.
//
// It runs kubeadm in KUBEADM_UPGRADE_ONLY mode: only `kubeadm upgrade apply` is
// executed (kubelet is upgraded separately via the OS profile). `kubeadm upgrade
// apply` is idempotent for an already-applied version, so the activity is safe to
// retry both within this workflow and across control-plane reconciles.
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

	var result activities.ExecuteClusterRunbookResult
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.ExecuteClusterRunbook,
		activities.ExecuteClusterRunbookInput{
			ClusterID:   input.ClusterID,
			RunbookName: "upgrade-kubelet-kubeadm",
			RunbookEnv: map[string]string{
				"VERSION":              input.KubernetesVersion,
				"IS_MASTER":            "true",
				"KUBEADM_UPGRADE_ONLY": "true",
			},
		},
	).Get(ctx, &result); err != nil {
		return UpgradeControlPlaneAddonsResult{}, fmt.Errorf("failed to upgrade control plane add-ons: %w", err)
	}

	logger.Info("Upgraded control plane add-ons",
		"clusterID", input.ClusterID,
		"kubernetesVersion", input.KubernetesVersion,
		"nodeID", result.NodeID,
		"nodeName", result.NodeName)

	return UpgradeControlPlaneAddonsResult{
		NodeID:   result.NodeID,
		NodeName: result.NodeName,
		Output:   result.Output,
	}, nil
}
