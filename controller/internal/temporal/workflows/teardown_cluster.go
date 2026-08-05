// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
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

	clusterCtx, err := getClusterProvisioningContext(ctx, input.ClusterID)
	if err != nil {
		return fmt.Errorf("failed to get cluster provisioning context: %w", err)
	}

	if err := cancelClusterProvisionWorkflow(ctx, input.ClusterID); err != nil {
		return fmt.Errorf("failed to cancel provision workflow: %w", err)
	}

	if err := teardownNodePools(ctx, input.ClusterID); err != nil {
		return fmt.Errorf("failed to teardown worker nodes: %w", err)
	}

	if err := deleteEtcdEncryptionKeySecret(ctx, input.ClusterID); err != nil {
		return fmt.Errorf("failed to delete etcd encryption key secret: %w", err)
	}

	if clusterCtx.InfraMode == activities.InfraModeCAPI {
		if err := workflow.ExecuteActivity(
			withK8sDeleteWaitOptions(ctx),
			a.DeleteCAPIClusterResources,
			activities.DeleteCAPIClusterResourcesInput{ClusterID: input.ClusterID},
		).Get(ctx, nil); err != nil {
			return fmt.Errorf("failed to delete CAPI cluster resources: %w", err)
		}
	}

	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.DeleteCluster, input.ClusterID).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to delete cluster: %w", err)
	}

	// Revoke the published tenant-access credentials last, after the cluster row
	// is gone. The tenant-access controller reconciles from a ListClusters
	// snapshot, so a pass that started before the row was deleted would otherwise
	// re-publish the Secrets we just removed and orphan them permanently. Deleting
	// the row first means no later pass can observe the cluster at all. The name
	// comes from the context read at the top of teardown, since the row no longer
	// exists to look it up.
	if err := deleteTenantAccessSecrets(ctx, input.ClusterID, clusterCtx.ClusterName); err != nil {
		return fmt.Errorf("failed to delete tenant-access secrets: %w", err)
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

func deleteTenantAccessSecrets(ctx workflow.Context, clusterID uuid.UUID, clusterName string) error {
	var a *activities.Activities

	if err := workflow.ExecuteActivity(
		withK8sDeleteWaitOptions(ctx),
		a.DeleteTenantAccessSecrets,
		activities.DeleteTenantAccessSecretsInput{
			ClusterID:   clusterID,
			ClusterName: clusterName,
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to delete tenant-access secrets: %w", err)
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
