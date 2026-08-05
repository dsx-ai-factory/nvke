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

type UpdateKubeadmConfigMapInput struct {
	ClusterID         uuid.UUID `json:"clusterID"`
	KubernetesVersion string    `json:"kubernetesVersion"`
}

type UpdateKubeadmConfigMapResult struct {
	// NodeID is the control plane node the runbook ran on.
	NodeID uuid.UUID `json:"nodeID"`
	// NodeName is the name of the control plane node the runbook ran on.
	NodeName string `json:"nodeName"`
	// Output is the stdout from the runbook execution.
	Output string `json:"output"`
}

// UpdateKubeadmConfigMap runs the embedded update-kubeadm-k8s-version runbook
// on a healthy control plane node via node-agent ExecuteScript.
func UpdateKubeadmConfigMap(ctx workflow.Context, input UpdateKubeadmConfigMapInput) (UpdateKubeadmConfigMapResult, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("Updating kubeadm ConfigMap",
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
			RunbookName: "update-kubeadm-k8s-version",
			RunbookEnv: map[string]string{
				"KUBERNETES_VERSION": input.KubernetesVersion,
			},
		},
	).Get(ctx, &result); err != nil {
		return UpdateKubeadmConfigMapResult{}, fmt.Errorf("failed to update kubeadm ConfigMap: %w", err)
	}

	logger.Info("Updated kubeadm ConfigMap",
		"clusterID", input.ClusterID,
		"kubernetesVersion", input.KubernetesVersion,
		"nodeID", result.NodeID,
		"nodeName", result.NodeName)

	return UpdateKubeadmConfigMapResult{
		NodeID:   result.NodeID,
		NodeName: result.NodeName,
		Output:   result.Output,
	}, nil
}
