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

type UpdateKubeadmConfigMapInput struct {
	ClusterID         uuid.UUID `json:"clusterID"`
	KubernetesVersion string    `json:"kubernetesVersion"`
}

type UpdateKubeadmConfigMapResult struct {
	Changed bool `json:"changed"`
}

// UpdateKubeadmConfigMap updates the tenant's kubeadm ClusterConfiguration
// through the injected long-lived tenant client.
func UpdateKubeadmConfigMap(ctx workflow.Context, input UpdateKubeadmConfigMapInput) (UpdateKubeadmConfigMapResult, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("Updating kubeadm ConfigMap",
		"clusterID", input.ClusterID,
		"kubernetesVersion", input.KubernetesVersion)

	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 30 * time.Second,
			MaximumInterval: 2 * time.Minute,
			MaximumAttempts: 3,
		},
	}

	var result activities.UpdateKubeadmKubernetesVersionResult
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.UpdateKubeadmKubernetesVersion,
		activities.UpdateKubeadmKubernetesVersionInput{
			ClusterID:         input.ClusterID,
			KubernetesVersion: input.KubernetesVersion,
		},
	).Get(ctx, &result); err != nil {
		return UpdateKubeadmConfigMapResult{}, fmt.Errorf("failed to update kubeadm ConfigMap: %w", err)
	}

	logger.Info("Updated kubeadm ConfigMap",
		"clusterID", input.ClusterID,
		"kubernetesVersion", input.KubernetesVersion,
		"changed", result.Changed)

	return UpdateKubeadmConfigMapResult{
		Changed: result.Changed,
	}, nil
}
