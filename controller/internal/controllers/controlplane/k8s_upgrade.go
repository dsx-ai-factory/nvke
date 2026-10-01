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

	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/temporal/workflows"
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
// It orchestrates the kubeadm ConfigMap update, OSProfile updates, and kubeadm upgrade apply.
func (r *reconciler) reconcileK8sUpgrade(ctx context.Context, cluster db.Cluster, cp *controlPlane) error {
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
		return r.startK8sUpgrade(ctx, cluster, cp, logger)
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
// 2. Updating OSProfiles for all control plane node pools (which makes old NodeSets deprecated)
// 3. Marking the cluster as upgrading and enabling control plane node recycling
func (r *reconciler) startK8sUpgrade(
	ctx context.Context,
	cluster db.Cluster,
	cp *controlPlane,
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

	// Update OSProfiles for node pools, which makes old NodeSets deprecated automatically.
	// Skipped under the control-plane-controller path: no CP node pools, and the new
	// version already lives in the cluster snapshot.
	if !r.cfg.UseControlPlaneControllerForControlPlane() {
		if err := r.updateNodePoolsKubeVersion(ctx, cluster, cp.nodePools, logger); err != nil {
			logger.Error("Failed to update OSProfile", "error", err)
			return err
		}
	}

	// Mark cluster as upgrading
	if err := r.updateClusterControlPlaneUpgradingCondition(ctx, cluster.ID, true, kubeadmConfigMapUpgradedMessagePrefix+cluster.KubernetesVersion, logger); err != nil {
		logger.Error("Failed to update cluster upgrading condition", "error", err)
		return err
	}
	// return nil to allow OS upgrade to start
	return nil
}

// isK8sVersionUpgradeNeeded checks if a cluster needs a Kubernetes version upgrade by examining
// the current NodePool versions in all control plane node pools.
// FIXME: https://github.com/dsx-ai-factory/nke/issues/937 - get k8s version with periodic
// health checks instead of relying on the OSProfile version.
func (r *reconciler) isK8sVersionUpgradeNeeded(
	cluster db.Cluster,
	cp *controlPlane,
	logger *slog.Logger,
) bool {
	// Control-plane-controller path: derive the need from each node's pinned
	// snapshot k8s version.
	if r.cfg.UseControlPlaneControllerForControlPlane() {
		needed := cp.controlPlaneControllerNeedsK8sUpgrade(cluster.KubernetesVersion)
		if needed {
			logger.Info("Control plane needs k8s upgrade", "targetVersion", cluster.KubernetesVersion)
		}
		return needed
	}

	if len(cp.nodePools) == 0 {
		logger.Debug("No control plane node pools found")
		return false
	}

	// Check if upgrade is needed by examining NodePool OSProfile/KubernetesVersion
	for _, nodePool := range cp.nodePools {
		if nodePool.KubernetesVersion != cluster.KubernetesVersion {
			logger.Info("NodePool needs upgrade",
				"nodePoolID", nodePool.ID,
				"nodePoolName", nodePool.Name,
				"currentNodePoolOSProfile", nodePoolOSProfileForLog(nodePool.NodePool),
				"currentVersion", nodePool.KubernetesVersion,
				"targetVersion", cluster.KubernetesVersion,
			)
			return true
		}
	}

	return false
}

// updateNodePoolsKubeVersion updates the OSProfile for node pools that need upgrading.
// It processes all node pools and updates those that don't match the cluster's target version.
func (r *reconciler) updateNodePoolsKubeVersion(
	ctx context.Context,
	cluster db.Cluster,
	cpNodePools []nodePool,
	logger *slog.Logger,
) error {
	for _, nodePool := range cpNodePools {
		nplogger := logger.With(
			"nodePoolID", nodePool.ID,
			"nodePoolName", nodePool.Name,
			"nodePoolOSProfile", nodePoolOSProfileForLog(nodePool.NodePool),
			"nodePoolKubernetesVersion", nodePool.KubernetesVersion,
			"targetVersion", cluster.KubernetesVersion,
		)

		if nodePool.KubernetesVersion == cluster.KubernetesVersion {
			// KubernetesVersion already correct - skip to next nodepool
			nplogger.Debug("NodePool KubernetesVersion already at target version - skipping sync")
			continue
		}

		nplogger.Info("Updating NodePool KubernetesVersion with cluster version")

		// Update this nodepool's KubernetesVersion
		if err := r.updateNodePoolKubeVersion(ctx, cluster, nodePool); err != nil {
			return fmt.Errorf("failed to sync OSProfile with cluster version: %s: %w", nodePool.ID, err)
		}
	}

	return nil
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
// - No deprecated nodes remain
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

	// Right after the pool version changes nothing is deprecated yet, so the
	// rollout count alone reads as done while every node still runs the old
	// kubeadm. Pool-managed only: the controller path never writes the pool
	// version, so a legacy pool would hold this above zero forever.
	if !cp.useController {
		if offVersion, pools, stuck := cp.nodesOffVersion(cluster.KubernetesVersion); offVersion > 0 {
			if stuck {
				logger.Warn("Nodes on the previous Kubernetes version in an auto-provision-disabled pool; the upgrade cannot finish",
					"nodesOffVersion", offVersion, "nodePoolIDs", pools)
			} else {
				logger.Info("Control plane nodes still on the previous Kubernetes version, upgrade not complete",
					"nodesOffVersion", offVersion, "nodePoolIDs", pools)
			}
			return false
		}
	}

	healthyCount := cp.countHealthyNodes()
	totalDesired := cp.countDesiredNodes()

	// Control-plane-controller path: require an exact match so any surge node is
	// scaled back down before completing.
	if cp.useController {
		if healthyCount != totalDesired {
			logger.Info("Control plane healthy count does not match desired, upgrade not complete",
				"healthyCount", healthyCount,
				"totalDesired", totalDesired)
			return false
		}
	} else if healthyCount < totalDesired {
		logger.Info("Control plane does not have desired healthy nodes, upgrade not complete",
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
		return false, fmt.Errorf("CWE Temporal client not available")
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

// updateNodePoolKubeVersion detects K8s version changes and updates NodePool OSProfile and/or KubernetesVersion.
// After updating the OSProfile and/or KubernetesVersion, the NodePool controller creates a new current NodeSet.
// Existing NodeSets become deprecated and their nodes are replaced during the rolling upgrade.
func (r *reconciler) updateNodePoolKubeVersion(
	ctx context.Context,
	cluster db.Cluster,
	nodePool nodePool,
) error {
	logger := r.logger.With(
		"clusterID", cluster.ID,
		"nodePoolID", nodePool.ID,
		"nodePoolName", nodePool.Name,
		"clusterK8sVersion", cluster.KubernetesVersion,
	)

	// Check if Kubernetes version update is needed
	if nodePool.KubernetesVersion == cluster.KubernetesVersion {
		logger.Debug("Node pool KubernetesVersion already matches cluster KubernetesVersion")
		return nil
	}

	logger.Info("Kubernetes version mismatch detected - updating NodePool KubernetesVersion and/or OSProfile",
		"targetVersion", cluster.KubernetesVersion,
		"nodePoolKubernetesVersion", nodePool.KubernetesVersion,
		"nodePoolOSProfile", nodePoolOSProfileForLog(nodePool.NodePool),
	)

	np, err := r.db.GetNodePool(ctx, nodePool.ID)
	if err != nil {
		return fmt.Errorf("failed to get node pool: %w", err)
	}

	upgradeConfig, err := r.nodePoolUpgradeConfig(cluster)
	if err != nil {
		return err
	}
	np, err = upgradeConfig.ResolveNodePoolUpgrade(ctx, r.k8sReader, np, cluster.KubernetesVersion, logger)
	if err != nil {
		return err
	}

	_, err = r.db.UpdateNodePool(ctx, &np)
	if err != nil {
		return fmt.Errorf("failed to update node pool OSProfile: %w", err)
	}

	logger.Info("Updated NodePool KubernetesVersion and/or OSProfile",
		"nodePoolOSProfile", nodePoolOSProfileForLog(np),
		"nodePoolKubernetesVersion", np.KubernetesVersion,
	)

	return nil
}

// nodePoolOSProfileForLog returns the OSProfile from the Nico provider payload
// for use in log attributes. Returns "" for CAPI pools (no OSProfile) or on error.
func nodePoolOSProfileForLog(nodePool db.NodePool) string {
	nicoNodePoolProvider, err := nodePool.NicoProvider()
	if err != nil {
		return ""
	}
	return nicoNodePoolProvider.OSProfile
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

	// The node-pool guard is spurious under the control-plane-controller path (no
	// CP node pools); the node guard below still applies.
	if !r.cfg.UseControlPlaneControllerForControlPlane() && len(cp.nodePools) == 0 {
		return false, fmt.Errorf("no control plane node pools found for cluster %s", cluster.ID)
	}

	if len(cp.nodes) == 0 {
		return false, fmt.Errorf("no nodes found in any control plane NodePool")
	}

	// Require controllerTemporalClient to be configured
	if r.controllerTemporalClient == nil {
		return false, fmt.Errorf("CWE Temporal client not available")
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

// startUpdateKubeadmConfigMapWorkflow starts the CWE-owned UpdateKubeadmConfigMap workflow.
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

	run, err := r.controllerTemporalClient.ExecuteWorkflow(ctx, options, workflows.UpdateKubeadmConfigMap, workflows.UpdateKubeadmConfigMapInput{
		ClusterID:         cluster.ID,
		KubernetesVersion: cluster.KubernetesVersion,
	})
	if err != nil {
		return fmt.Errorf("failed to start UpdateKubeadmConfigMap workflow: %w", err)
	}

	logger.Info("Started UpdateKubeadmConfigMap workflow - will check status on next reconcile",
		"workflowID", run.GetID(),
		"runID", run.GetRunID())

	return nil
}

// startKubeadmAddOnUpgradeWorkflow starts the CWE-owned UpgradeControlPlaneAddons
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

	run, err := r.controllerTemporalClient.ExecuteWorkflow(ctx, options, workflows.UpgradeControlPlaneAddons, workflows.UpgradeControlPlaneAddonsInput{
		ClusterID:         cluster.ID,
		KubernetesVersion: cluster.KubernetesVersion,
	})
	if err != nil {
		return fmt.Errorf("failed to start kubeadm upgrade workflow: %w", err)
	}

	logger.Info("Started kubeadm upgrade workflow - will check status on next reconcile",
		"workflowID", run.GetID(),
		"runID", run.GetRunID(),
		"kubernetesVersion", cluster.KubernetesVersion)

	// Workflow started - not yet completed
	return nil
}
