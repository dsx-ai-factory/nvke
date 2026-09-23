// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/temporal/activities"
)

const (
	// runbookEnvTrue is the value runbooks expect for a boolean environment flag.
	runbookEnvTrue = "true"
)

// Add ActivityOptions to the context, tuned for database interactions that should be fast.
// Indefinite retry with backoff to handle transient database errors.
func withDBOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			MaximumInterval:    1 * time.Minute,
			BackoffCoefficient: 2,
			MaximumAttempts:    0, // indefinite retry
		},
	})
}

// Add ActivityOptions for Kubernetes deletions that may block on finalizers.
func withK8sDeleteWaitOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 20 * time.Minute,
		HeartbeatTimeout:    30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			MaximumInterval:    1 * time.Minute,
			BackoffCoefficient: 2,
			MaximumAttempts:    0,
		},
	})
}

type nodeMetricLabels struct {
	ClusterName string
	NodeType    string
}

func getNodeMetricLabels(ctx workflow.Context, nodeID uuid.UUID) (nodeMetricLabels, error) {
	var a *activities.Activities

	var nodePool db.NodePool
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.GetNodePoolForNode, nodeID).Get(ctx, &nodePool); err != nil {
		return nodeMetricLabels{}, fmt.Errorf("failed to get node pool: %w", err)
	}

	var cluster db.Cluster
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.GetClusterForNode, nodeID).Get(ctx, &cluster); err != nil {
		return nodeMetricLabels{}, fmt.Errorf("failed to get cluster: %w", err)
	}

	nodeType := "worker"
	if nodePool.ControlPlane {
		nodeType = "cp"
	}

	return nodeMetricLabels{ClusterName: cluster.Name, NodeType: nodeType}, nil
}
