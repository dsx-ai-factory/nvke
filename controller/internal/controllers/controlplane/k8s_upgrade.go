// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/otelx"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/workflows"
)

const (
	kubeadmConfigMapUpgradedMessagePrefix = "kubeadm configmap upgraded to "
)

// isWorkflowNotFoundError checks if an error is a Temporal "workflow not found" error.
func isWorkflowNotFoundError(err error) bool {
	var notFound *serviceerror.NotFound
	return errors.As(err, &notFound)
}

// reconcileK8sUpgrade handles Kubernetes version upgrades for a cluster.
// It orchestrates the kubeadm ConfigMap update and kubeadm upgrade apply.
func (r *reconciler) reconcileK8sUpgrade(
	ctx context.Context,
	cluster db.Cluster,
	cp *controlPlane,
) error {
	logger := r.logger.With(
		"clusterID", cluster.ID,
		"clusterName", cluster.Name,
		"kubernetesVersion", cluster.KubernetesVersion,
	)

	// If K8s upgrade is in progress, check if it's complete
	isK8sVersionUpgradeInProgress := cluster.Conditions.ControlPlaneUpgrading.Status == db.ConditionStatusTrue
	if isK8sVersionUpgradeInProgress {
		return r.reconcileUpgradeInProgress(ctx, cluster, cp, logger)
	}

	// If K8s upgrade is needed and not yet started
	isK8sVersionUpgradeNeeded := r.isK8sVersionUpgradeNeeded(cluster, cp, logger)
	if isK8sVersionUpgradeNeeded {
		return r.startK8sUpgrade(ctx, cluster, logger)
	}

	return nil
}

// reconcileUpgradeInProgress handles the reconciliation when a control plane upgrade is in progress.
// It checks if the upgrade is complete and if so, applies kubeadm upgrade and marks the upgrade as complete.
func (r *reconciler) reconcileUpgradeInProgress(
	ctx context.Context,
	cluster db.Cluster,
	cp *controlPlane,
	logger *slog.Logger,
) error {
	upgradeComplete := r.isControlPlaneUpgradeComplete(cluster, cp, logger)

	if !upgradeComplete {
		// Upgrade still in progress, nothing to do
		return nil
	}

	// Apply kubeadm upgrade to update add-ons
	addonsComplete, err := r.upgradeAddons(ctx, cluster, cp)
	if err != nil {
		logger.Error("Failed to upgrade add-ons", "error", err)
		return err
	}
	if !addonsComplete {
		logger.Info("Add-ons upgrade workflow is in progress - waiting for completion")
		return nil
	}
	logger.Info("Add-ons upgrade workflow completed successfully", "kubernetesVersion", cluster.KubernetesVersion)

	// Mark upgrade as complete
	if err := r.updateClusterControlPlaneUpgradingCondition(ctx, cluster.ID, false, "Control plane upgrade completed successfully", logger); err != nil {
		logger.Error("Failed to clear cluster upgrading condition", "error", err)
		return err
	}

	return nil
}

// startK8sUpgrade initiates a Kubernetes version upgrade by:
// 1. Updating the kubeadm ConfigMap to the target version
// 2. Marking the cluster as upgrading and enabling control plane node recycling
func (r *reconciler) startK8sUpgrade(
	ctx context.Context,
	cluster db.Cluster,
	logger *slog.Logger,
) error {
	// Update kubeadm configmap version to the cluster's Kubernetes version
	configMapComplete, err := r.updateKubeadmConfigMap(ctx, cluster)
	if err != nil {
		logger.Error("Failed to update kubeadm ConfigMap", "error", err)
		return err
	}
	if !configMapComplete {
		logger.Info("K8s upgrade workflow in progress - will check status on next reconcile")
		return nil
	}
	logger.Info("Updated kubeadm ConfigMap", "kubernetesVersion", cluster.KubernetesVersion)

	// Mark cluster as upgrading
	if err := r.updateClusterControlPlaneUpgradingCondition(ctx, cluster.ID, true, kubeadmConfigMapUpgradedMessagePrefix+cluster.KubernetesVersion, logger); err != nil {
		logger.Error("Failed to update cluster upgrading condition", "error", err)
		return err
	}
	// return nil to allow OS upgrade to start
	return nil
}

// isK8sVersionUpgradeNeeded checks if a cluster needs a Kubernetes version upgrade by comparing
// each control-plane node's pinned snapshot version against the cluster's target.
// FIXME: https://github.com/dsx-ai-factory/nvke/issues/937 - get k8s version with periodic
// health checks instead of relying on the OSProfile version.
func (r *reconciler) isK8sVersionUpgradeNeeded(
	cluster db.Cluster,
	cp *controlPlane,
	logger *slog.Logger,
) bool {
	needed := cp.controlPlaneControllerNeedsK8sUpgrade(cluster.KubernetesVersion)
	if needed {
		logger.Info("Control plane needs k8s upgrade", "targetVersion", cluster.KubernetesVersion)
	}
	return needed
}

// updateClusterControlPlaneUpgradingCondition updates the cluster's control plane upgrading condition with the given status and message.
func (r *reconciler) updateClusterControlPlaneUpgradingCondition(
	ctx context.Context,
	clusterID uuid.UUID,
	status bool,
	message string,
	logger *slog.Logger,
) error {
	cluster, err := r.db.GetCluster(ctx, clusterID)
	if err != nil {
		return fmt.Errorf("failed to get cluster: %w", err)
	}

	cluster.Conditions.ControlPlaneUpgrading = db.NewCondition(status, message)
	_, err = r.db.UpdateCluster(ctx, &cluster)
	if err != nil {
		return fmt.Errorf("failed to update cluster: %w", err)
	}

	logger.Info("Updated cluster control plane upgrading condition", "status", status, "message", message)

	return nil
}

// isControlPlaneUpgradeComplete checks if the cluster upgrade is complete by verifying:
// - CPNodesRecycling condition is not True (node recycling is complete)
// - No nodes need rollout
// - All desired healthy nodes are present
func (r *reconciler) isControlPlaneUpgradeComplete(
	cluster db.Cluster,
	cp *controlPlane,
	logger *slog.Logger,
) bool {
	if cluster.Conditions.CPNodesRecycling.Status == db.ConditionStatusTrue {
		logger.Info("Control plane nodes recycling in progress",
			"cpNodesRecyclingStatus", cluster.Conditions.CPNodesRecycling.Status)
		return false
	}

	rolloutCount := cp.countNodesNeedingRollout()

	if rolloutCount > 0 {
		logger.Info("Cluster has nodes needing rollout, upgrade not complete",
			"nodesNeedingRolloutCount", rolloutCount)
		return false
	}

	healthyCount := cp.countHealthyNodes()
	totalDesired := cp.countDesiredNodes()

	// Exact match so any surge node is scaled back down before completing.
	if healthyCount != totalDesired {
		logger.Info("Control plane healthy count does not match desired, upgrade not complete",
			"healthyCount", healthyCount,
			"totalDesired", totalDesired)
		return false
	}

	logger.Info("Cluster upgrade complete - all checks passed",
		"clusterID", cluster.ID,
		"clusterName", cluster.Name,
		"healthyCount", healthyCount,
		"totalDesired", totalDesired,
		"targetK8sVersion", cluster.KubernetesVersion)

	return true
}

// updateKubeadmConfigMap updates the kubeadm configmap in the tenant cluster.
// Returns (completed, error) where completed indicates if the update is done.
func (r *reconciler) updateKubeadmConfigMap(ctx context.Context, cluster db.Cluster) (bool, error) {
	logger := r.logger.With(
		"clusterID", cluster.ID,
		"kubernetesVersion", cluster.KubernetesVersion,
	)

	if r.controllerTemporalClient == nil {
		return false, fmt.Errorf("controller Temporal client not available")
	}

	// Use a stable workflow ID to track across reconciles
	workflowID := fmt.Sprintf("%s%s-%s", constants.UpdateKubeadmCMWorkflowPrefix, cluster.ID, cluster.KubernetesVersion)

	// Check if workflow is already running or completed
	desc, err := r.controllerTemporalClient.DescribeWorkflowExecution(ctx, workflowID, "")

	if isWorkflowNotFoundError(err) {
		if err := r.startUpdateKubeadmConfigMapWorkflow(ctx, cluster, workflowID, logger); err != nil {
			return false, err
		}
		// Check status on next reconcile
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("failed to describe workflow execution: %w", err)
	}

	// No error - workflow exists, check its status
	if desc.GetWorkflowExecutionInfo().GetCloseTime() == nil {
		// Workflow is still running
		logger.Info("UpdateKubeadmConfigMap workflow is still running - waiting for completion",
			"workflowID", workflowID)
		return false, nil
	}

	// Workflow completed - check if it succeeded
	if desc.GetWorkflowExecutionInfo().GetStatus() == enums.WORKFLOW_EXECUTION_STATUS_COMPLETED {
		logger.Info("UpdateKubeadmConfigMap workflow completed successfully",
			"workflowID", workflowID)
		return true, nil
	}

	// Workflow failed or timed out - start a new run to retry. DescribeWorkflowExecution
	// keeps returning the closed execution, so without this the ConfigMap update would
	// stay wedged after a transient failure instead of reaching the not-found branch.
	logger.Warn("UpdateKubeadmConfigMap workflow failed - restarting", "workflowID", workflowID, "status", desc.GetWorkflowExecutionInfo().GetStatus().String())
	if err := r.startUpdateKubeadmConfigMapWorkflow(ctx, cluster, workflowID, logger); err != nil {
		return false, err
	}

	return false, nil
}

// upgradeAddons runs kubeadm upgrade apply on a control plane node to upgrade add-ons
// (kube-proxy, CoreDNS) to match the cluster's Kubernetes version.
// upgradeAddons runs kubeadm upgrade apply to update cluster add-ons.
// Returns (completed, error) where completed indicates if the upgrade is done.
func (r *reconciler) upgradeAddons(
	ctx context.Context,
	cluster db.Cluster,
	cp *controlPlane,
) (bool, error) {
	logger := r.logger.With(
		"clusterID", cluster.ID,
		"clusterName", cluster.Name,
		"kubernetesVersion", cluster.KubernetesVersion,
	)

	if len(cp.nodes) == 0 {
		return false, fmt.Errorf("no control plane nodes found for cluster %s", cluster.ID)
	}

	// Require controllerTemporalClient to be configured
	if r.controllerTemporalClient == nil {
		return false, fmt.Errorf("controller Temporal client not available")
	}

	// Note: Temporal allows multiple runs with the same workflow ID.
	// DescribeWorkflowExecution without a runID returns the most recent execution.
	// When a workflow fails, we start a new execution with the same workflow ID but different runID.
	workflowID := fmt.Sprintf("%s%s-%s", constants.UpgradeControlPlaneAddonsWorkflowPrefix, cluster.ID, cluster.KubernetesVersion)

	// Check if workflow is already running or completed
	// This returns the most recent execution for this workflow ID
	desc, err := r.controllerTemporalClient.DescribeWorkflowExecution(ctx, workflowID, "")

	if isWorkflowNotFoundError(err) {
		if err := r.startKubeadmAddOnUpgradeWorkflow(ctx, cluster, workflowID, logger); err != nil {
			return false, err
		}
		// Check status on next reconcile
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("failed to describe workflow execution: %w", err)
	}

	// No error - workflow exists, check its status
	if desc.GetWorkflowExecutionInfo().GetCloseTime() == nil {
		// Workflow is still running
		logger.Info("Kubeadm upgrade workflow is still running - waiting for completion",
			"workflowID", workflowID)
		return false, nil
	}

	// Workflow completed - check if it succeeded
	if desc.GetWorkflowExecutionInfo().GetStatus() == enums.WORKFLOW_EXECUTION_STATUS_COMPLETED {
		logger.Info("Kubeadm upgrade workflow completed successfully",
			"workflowID", workflowID)
		return true, nil
	}

	// Workflow failed or timed out - start a new run to retry. The previous execution is
	// closed, so ExecuteWorkflow (WorkflowIDReusePolicy ALLOW_DUPLICATE_FAILED_ONLY) starts a
	// fresh run under the same workflow ID. Without this, a transient failure (e.g. a control
	// plane VIP blip during kubeadm's health preflight) would wedge the upgrade indefinitely,
	// since the not-found branch above never fires for a closed-but-failed workflow.
	logger.Warn("Kubeadm upgrade workflow failed - restarting", "workflowID", workflowID, "status", desc.GetWorkflowExecutionInfo().GetStatus().String())
	if err := r.startKubeadmAddOnUpgradeWorkflow(ctx, cluster, workflowID, logger); err != nil {
		return false, err
	}
	return false, nil
}

// startUpdateKubeadmConfigMapWorkflow starts the controller-owned UpdateKubeadmConfigMap workflow.
func (r *reconciler) startUpdateKubeadmConfigMapWorkflow(ctx context.Context, cluster db.Cluster, workflowID string, logger *slog.Logger) error {
	logger.Info("Starting UpdateKubeadmConfigMap workflow")

	options := client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: r.cfg.TemporalTaskQueue,
		// If a workflow with this ID is already running, use it
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		// Only allow starting a new workflow if the previous one failed
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
		// Must exceed activity worst-case budget (3x10m + retry backoff + overhead).
		WorkflowRunTimeout: 35 * time.Minute,
	}

	ctx, span := otelx.StartWorkflowSpan(ctx, otelx.WorkflowStart, otelx.WorkflowAttributes{
		WorkflowType: workflows.UpdateKubeadmConfigMapWorkflowType,
		WorkflowID:   workflowID,
		ClusterID:    cluster.ID,
		ClusterName:  cluster.Name,
	})
	run, err := r.controllerTemporalClient.ExecuteWorkflow(ctx, options, workflows.UpdateKubeadmConfigMap, workflows.UpdateKubeadmConfigMapInput{
		ClusterID:         cluster.ID,
		KubernetesVersion: cluster.KubernetesVersion,
	})
	otelx.EndWorkflowSpan(span, err)
	if err != nil {
		return fmt.Errorf("failed to start UpdateKubeadmConfigMap workflow: %w", err)
	}

	logger.InfoContext(ctx, "Started UpdateKubeadmConfigMap workflow - will check status on next reconcile",
		"workflow_id", workflowID,
		"run_id", run.GetRunID())

	return nil
}

// startKubeadmAddOnUpgradeWorkflow starts the controller-owned UpgradeControlPlaneAddons
// workflow, which runs kubeadm upgrade apply through the node agent on a healthy
// control-plane node selected by the activity.
func (r *reconciler) startKubeadmAddOnUpgradeWorkflow(ctx context.Context, cluster db.Cluster, workflowID string, logger *slog.Logger) error {
	// Start workflow asynchronously - don't wait for completion
	options := client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: r.cfg.TemporalTaskQueue,
		// Must exceed activity worst-case budget (3x10m + retry backoff + overhead).
		WorkflowRunTimeout: 35 * time.Minute,
		// If a workflow with this ID is already running (race condition), use it
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		// Only allow starting a new workflow if the previous one failed or timed out
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
	}

	ctx, span := otelx.StartWorkflowSpan(ctx, otelx.WorkflowStart, otelx.WorkflowAttributes{
		WorkflowType: workflows.UpgradeControlPlaneAddonsWorkflowType,
		WorkflowID:   workflowID,
		ClusterID:    cluster.ID,
		ClusterName:  cluster.Name,
	})
	run, err := r.controllerTemporalClient.ExecuteWorkflow(ctx, options, workflows.UpgradeControlPlaneAddons, workflows.UpgradeControlPlaneAddonsInput{
		ClusterID:         cluster.ID,
		KubernetesVersion: cluster.KubernetesVersion,
	})
	otelx.EndWorkflowSpan(span, err)
	if err != nil {
		return fmt.Errorf("failed to start kubeadm upgrade workflow: %w", err)
	}

	logger.InfoContext(ctx, "Started kubeadm upgrade workflow - will check status on next reconcile",
		"workflow_id", workflowID,
		"run_id", run.GetRunID(),
		"kubernetes_version", cluster.KubernetesVersion)

	// Workflow started - not yet completed
	return nil
}
