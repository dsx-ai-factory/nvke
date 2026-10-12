// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

type TeardownClusterInput struct {
	ClusterID uuid.UUID
}

func TeardownCluster(ctx workflow.Context, input TeardownClusterInput) error {
	logger := workflow.GetLogger(ctx)

	if err := teardownCluster(ctx, input); err != nil {
		// Best effort to update the cluster conditions to indicate the teardown workflow failed
		if err := updateClusterDeletingMessage(ctx, input.ClusterID, fmt.Sprintf("Workflow failed: %s", err)); err != nil {
			logger.Error("failed to update cluster conditions", "error", err)
		}

		return fmt.Errorf("failed to teardown cluster: %w", err)
	}

	return nil
}

func teardownCluster(ctx workflow.Context, input TeardownClusterInput) error {
	var a *activities.Activities

	if input.ClusterID == uuid.Nil {
		return errors.New("clusterID is required")
	}

	if err := cancelClusterProvisionWorkflow(ctx, input.ClusterID); err != nil {
		return fmt.Errorf("failed to cancel provision workflow: %w", err)
	}

	if err := teardownNodePools(ctx, input.ClusterID); err != nil {
		return fmt.Errorf("failed to teardown worker nodes: %w", err)
	}

	// Control-plane-controller-managed CP nodes aren't owned by a node pool, so
	// teardownNodePools misses them. Delete them before DeleteCluster or the nodes
	// foreign key blocks cluster deletion and the workflow retries forever.
	if err := teardownControlPlaneNodes(ctx, input.ClusterID); err != nil {
		return fmt.Errorf("failed to teardown control-plane nodes: %w", err)
	}

	if err := deleteEtcdEncryptionKeySecret(ctx, input.ClusterID); err != nil {
		return fmt.Errorf("failed to delete etcd encryption key secret: %w", err)
	}

	if err := workflow.ExecuteActivity(
		withK8sDeleteWaitOptions(ctx),
		a.DeleteCAPIClusterResources,
		activities.DeleteCAPIClusterResourcesInput{ClusterID: input.ClusterID},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to delete CAPI cluster resources: %w", err)
	}

	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.DeleteCluster, input.ClusterID).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to delete cluster: %w", err)
	}

	return nil
}

func cancelClusterProvisionWorkflow(ctx workflow.Context, clusterID uuid.UUID) error {
	if err := updateClusterDeletingMessage(ctx, clusterID, "Canceling provision workflow"); err != nil {
		return fmt.Errorf("failed to update cluster conditions: %w", err)
	}

	// Cancel the provision workflow if it exists
	provisionWorkflowID := fmt.Sprintf("%s%s", constants.ProvisionClusterWorkflowPrefix, clusterID)
	if err := workflow.RequestCancelExternalWorkflow(ctx, provisionWorkflowID, "").Get(ctx, nil); err != nil {
		var notFound *temporal.UnknownExternalWorkflowExecutionError
		if !errors.As(err, &notFound) {
			return fmt.Errorf("failed to cancel provision workflow: %w", err)
		}
		// Workflow doesn't exist, which we'll consider to be a successful cancellation
	}

	return nil
}

func teardownNodePools(ctx workflow.Context, clusterID uuid.UUID) error {
	var a *activities.Activities

	if err := workflow.ExecuteActivity(
		withDBOptions(ctx),
		a.TriggerClusterTeardownNodePools,
		activities.TriggerClusterTeardownNodePoolsInput{
			ClusterID:                       clusterID,
			SkipRemoveFromCluster:           true, // We're deleting the cluster, so we don't need to remove the node pools
			ClusterDeletingConditionMessage: "Tearing down node pools",
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to trigger teardown of cluster node pools: %w", err)
	}

	// Monitor the database until all node pools are deleted
	waitOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 20 * time.Minute,
		HeartbeatTimeout:    2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: time.Second,
			MaximumInterval: time.Minute,
			MaximumAttempts: 0,
		},
	}
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, waitOptions),
		a.WaitForClusterNodePoolsDeleted,
		activities.WaitForClusterNodePoolsDeletedInput{
			ClusterID:                       clusterID,
			ClusterDeletingConditionMessage: "Waiting for node pools to be deleted",
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to monitor node pools deletion: %w", err)
	}

	return nil
}

func teardownControlPlaneNodes(ctx workflow.Context, clusterID uuid.UUID) error {
	var a *activities.Activities

	if err := workflow.ExecuteActivity(
		withDBOptions(ctx),
		a.TriggerClusterTeardownControlPlaneNodes,
		activities.TriggerClusterTeardownControlPlaneNodesInput{
			ClusterID:                       clusterID,
			ClusterDeletingConditionMessage: "Tearing down control-plane nodes",
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to trigger teardown of cluster control-plane nodes: %w", err)
	}

	// Monitor the database until all cluster-level control-plane nodes are deleted.
	waitOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 20 * time.Minute,
		HeartbeatTimeout:    2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: time.Second,
			MaximumInterval: time.Minute,
			MaximumAttempts: 0,
		},
	}
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, waitOptions),
		a.WaitForClusterControlPlaneNodesDeleted,
		activities.WaitForClusterControlPlaneNodesDeletedInput{
			ClusterID:                       clusterID,
			ClusterDeletingConditionMessage: "Waiting for control-plane nodes to be deleted",
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to monitor control-plane nodes deletion: %w", err)
	}

	return nil
}

func deleteEtcdEncryptionKeySecret(ctx workflow.Context, clusterID uuid.UUID) error {
	var a *activities.Activities

	if err := workflow.ExecuteActivity(
		withK8sDeleteWaitOptions(ctx),
		a.DeleteEtcdEncryptionKeySecret,
		activities.DeleteEtcdEncryptionKeySecretInput{
			ClusterID:                       clusterID,
			ClusterDeletingConditionMessage: "Deleting etcd encryption key secret",
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to delete etcd encryption key secret: %w", err)
	}

	return nil
}

func updateClusterDeletingMessage(ctx workflow.Context, clusterID uuid.UUID, message string) error {
	var a *activities.Activities

	return workflow.ExecuteActivity(withDBOptions(ctx), a.UpdateClusterConditions, activities.ClusterConditionsUpdate{
		ClusterID: clusterID,
		Conditions: activities.ClusterConditions{
			Deleting: &activities.ConditionUpdate{
				Status:  db.ConditionStatusTrue,
				Message: message,
			},
		},
	}).Get(ctx, nil)
}
