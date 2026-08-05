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
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/metrics"
	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

type RebootNodeInput struct {
	NodeID           uuid.UUID
	Reason           string
	InitialTimeout   time.Duration
	TimeoutIncrement time.Duration
	MaxTimeout       time.Duration
	PollingInterval  time.Duration
}

func RebootNode(ctx workflow.Context, input RebootNodeInput) (wfErr error) {
	// Developer note:
	// [NKE Node Remediation SADD](https://docs.google.com/document/d/1jcPbeJ37yC4iesrf6wx9QyZlkCnYOJJIJO5R192C2fY/edit?tab=t.0#heading=h.dh5skq4lgud6)
	//
	// This workflow is depended upon by:
	// - [NVSentinel](https://gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel)
	// - [dgxc-janitor](https://gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/dgxc-janitor)
	// Please discuss any label or workflow changes with [#dgxc-gpu-node-resilience](https://nvidia.enterprise.slack.com/archives/C08E8U45VV4)
	//
	// Forge label-driven reboots try SoftRebootNode (in-guest reboot runbook) first and
	// fall back to Forge TriggerInstanceReboot only if the runbook cannot be executed.
	// Hard Forge reboot is out-of-band (no guest sync/unmount); see NKX-13717 / NKX-13323.
	// CAPI path is unchanged.

	logger := workflow.GetLogger(ctx)
	logger.Info("Starting reboot node workflow", "nodeID", input.NodeID)

	workflowStartTime := workflow.Now(ctx).UTC()

	var node corev1.Node

	nodeCtx, err := getNodeProvisioningContext(ctx, input.NodeID)
	if err != nil {
		return fmt.Errorf("failed to get node provisioning context: %w", err)
	}

	defer func() {
		// Clean up if the workflow is canceled
		if node.Name != "" {
			metrics.ClearCurrentAttempt(node.Name)
		}
		// Note we are using a named return here, which I generally dislike, but since this workflow
		// will cancel itself if the user removes the reboot label, this defer statement will run
		// before the cancel is processed by temporal, so this is the only way to observe the cancel in that case.
		if errors.Is(ctx.Err(), workflow.ErrCanceled) || errors.Is(wfErr, workflow.ErrCanceled) {
			logger.Info("Workflow canceled, cleaning up", "nodeID", input.NodeID, "nodeName", node.Name)
			newCtx, _ := workflow.NewDisconnectedContext(ctx)
			err := completeReboot(newCtx, input, constants.RebootStatusCanceled, nil, nodeCtx)
			if err != nil {
				logger.Error("failed to clean up canceled workflow", "error", err, "nodeID", input.NodeID, "nodeName", node.Name)
			}
		}
	}()

	if input.NodeID == uuid.Nil {
		return errors.New("nodeID is required")
	}

	// Check if node is a CP node
	isCPNode, err := isControlPlaneNode(ctx, input.NodeID)
	if err != nil {
		return fmt.Errorf("failed to check if node is a control-plane node: %w", err)
	}

	// if CP node, remove reboot label and finish workflow with success
	if isCPNode {
		logger.Warn("Skipping reboot: Node is a control-plane node. label based reboots are not allowed for control-plane nodes", "nodeID", input.NodeID)
		return completeReboot(ctx, input, constants.RebootStatusNotAllowed, nil, nodeCtx)
	}

	// Set reboot-started label
	// The reboot-started is an ephemeral label, and will be deleted after the node has rebooted.
	rebootStartedLabel := fmt.Sprintf("%s=%s", constants.LabelKeyNodeLabelRebootStarted, formatTimeForK8sLabel(workflow.Now(ctx).UTC()))

	// Also set reboot-status labels
	// They are used to provide external visibility to SREs and engineers that the reboot is in progress,
	// and making progress (or at least retrying) on the reboot, using kube native labels.
	// Instead of needing access to our logging and monitoring.
	rebootStatusLabel := fmt.Sprintf("%s=%s", constants.LabelKeyNodeLabelRebootStatus, constants.RebootStatusInProgress)
	rebootStatusTime := fmt.Sprintf("%s=%s", constants.LabelKeyNodeLabelRebootStatusTimestamp, formatTimeForK8sLabel(time.Now().UTC()))

	if err := applyRebootLabelsAndSetNodeCondition(ctx, input, rebootStartedLabel, rebootStatusLabel, rebootStatusTime); err != nil {
		return fmt.Errorf("failed to apply reboot-started label to node: %w", err)
	}

	// Try to reboot the node forever, until it succeeds.
	for rebootAttempt := 1; ; rebootAttempt++ {
		// Get the current node json info
		node, err = getNode(ctx, input.NodeID)
		if err != nil {
			return fmt.Errorf("failed to get node: %w", err)
		}

		// If the reboot label is not present, assume the user wishes to cancel the reboot.
		if node.Labels[constants.LabelKeyNodeLabelReboot] == "" {
			logger.Info("Node reboot label removed, canceling workflow", "nodeID", input.NodeID, "nodeName", node.Name)
			return workflow.ErrCanceled // Cancel the workflow cleanup happens in defer
		}

		if !workflow.IsReplaying(ctx) {
			metrics.SetCurrentAttempt(node.Name, rebootAttempt)
		}

		// Update reboot attempt label
		rebootAttemptLabel := fmt.Sprintf("%s=%d", constants.LabelKeyNodeLabelRebootAttempt, rebootAttempt)
		if err := applyRebootLabelsAndSetNodeCondition(ctx, input, rebootAttemptLabel); err != nil {
			return fmt.Errorf("failed to apply reboot-attempt label to node: %w", err)
		}

		// Use capi or forge to reboot the node/instance
		logger.Info("Rebooting node", "nodeID", input.NodeID, "nodeName", node.Name, "rebootAttempt", rebootAttempt, "reason", input.Reason, "infraMode", nodeCtx.InfraMode)
		rebootStartedTime, err := rebootNode(ctx, nodeCtx, rebootAttempt, input.Reason)
		if err != nil {
			var appErr *temporal.ApplicationError
			if errors.As(err, &appErr) && strings.EqualFold(appErr.Type(), "InstanceNotFound") {
				logger.Warn("Forge instance not found, completing reboot with failed status", "nodeID", input.NodeID, "nodeName", node.Name, "rebootAttempt", rebootAttempt)
				if cleanupErr := completeReboot(ctx, input, constants.RebootStatusFailedInstanceNotFound, nil, nodeCtx); cleanupErr != nil {
					return errors.Join(fmt.Errorf("failed to reboot node: %w", err), cleanupErr)
				}
			}
			return fmt.Errorf("failed to reboot node: %w", err)
		}

		// Wait for the node to reboot
		currentBootID := node.Status.NodeInfo.BootID
		if err := waitForNodeReboot(ctx, input, rebootAttempt, currentBootID, rebootStartedTime); err != nil {
			// If the workflow was canceled, return the error to the caller
			if errors.Is(ctx.Err(), workflow.ErrCanceled) {
				return ctx.Err()
			}
			// Otherwise, log the error and continue to the next attempt
			logger.Error("failed to wait for node reboot", "error", err, "nodeID", input.NodeID, "nodeName", node.Name, "rebootAttempt", rebootAttempt)
			continue
		}
		logger.Info("Node booted successfully", "nodeID", input.NodeID, "nodeName", node.Name, "rebootAttempt", rebootAttempt)

		return completeReboot(ctx, input, constants.RebootStatusCompleted, &rebootSuccessDetails{
			node:              node,
			workflowStartTime: workflowStartTime,
			rebootAttempt:     rebootAttempt,
		}, nodeCtx)
	}
}

type rebootSuccessDetails struct {
	node              corev1.Node
	workflowStartTime time.Time
	rebootAttempt     int
}

// completeReboot applies terminal reboot labels and clears the rebooting DB condition.
// Pass success non-nil only after a successful reboot: sets last-completed-reboot, records metrics, and logs completion.
func completeReboot(ctx workflow.Context, input RebootNodeInput, status constants.RebootStatus, success *rebootSuccessDetails, nodeCtx activities.NodeProvisioningContext) error {
	logger := workflow.GetLogger(ctx)

	ts := formatTimeForK8sLabel(time.Now().UTC())

	var conditionMsg string
	if success != nil {
		conditionMsg = "Node successfully rebooted at " + ts
	} else {
		conditionMsg = "Node reboot " + string(status) + " at " + ts
	}

	if nodeCtx.InfraMode == activities.InfraModeCAPI {
		if err := deleteCAPIMachineRebootAnnotation(ctx, input.NodeID); err != nil {
			return fmt.Errorf("failed to delete capi machine reboot annotation: %w", err)
		}
	}

	rebootStatusLabel := fmt.Sprintf("%s=%s", constants.LabelKeyNodeLabelRebootStatus, status)
	rebootStatusTime := fmt.Sprintf("%s=%s", constants.LabelKeyNodeLabelRebootStatusTimestamp, ts)
	deleteRebootStartedLabel := fmt.Sprintf("%s-", constants.LabelKeyNodeLabelRebootStarted)
	deleteRebootLabel := fmt.Sprintf("%s-", constants.LabelKeyNodeLabelReboot)
	deleteRebootAttemptLabel := fmt.Sprintf("%s-", constants.LabelKeyNodeLabelRebootAttempt)

	var labelArgs []string
	labelArgs = append(labelArgs, rebootStatusLabel, rebootStatusTime)

	if success != nil {
		lastRebootedAtLabel := fmt.Sprintf("%s=%s", constants.LabelKeyNodeLabelLastCompletedReboot, ts)
		labelArgs = append(labelArgs, lastRebootedAtLabel, deleteRebootStartedLabel, deleteRebootLabel, deleteRebootAttemptLabel)
	} else {
		labelArgs = append(labelArgs, deleteRebootStartedLabel, deleteRebootLabel, deleteRebootAttemptLabel)
	}

	if err := applyRebootLabelsAndSetNodeCondition(ctx, input, labelArgs...); err != nil {
		return fmt.Errorf("failed to apply completion labels to node: %w", err)
	}

	if err := updateNodeRebootingCondition(ctx, input.NodeID, db.ConditionStatusFalse, conditionMsg); err != nil {
		return fmt.Errorf("failed to update node conditions: %w", err)
	}

	if success != nil {
		duration := workflow.Now(ctx).Sub(success.workflowStartTime)
		if !workflow.IsReplaying(ctx) {
			rebootReason := success.node.Labels[constants.LabelKeyNodeLabelReboot]
			metrics.RecordRebootCompleted(rebootReason, duration, success.rebootAttempt)
		}
		logger.Info("Node reboot complete", "nodeID", input.NodeID, "nodeName", success.node.Name, "rebootAttempt", success.rebootAttempt, "duration", duration.Seconds())
	}

	return nil
}

// ISO datetime strings cannot be used as-is for labels, as they contain colons.
// A label format has already been described in mk8s: Node Management Guidelines:
// See: https://dgxcloud.gitlab-master-pages.nvidia.com/mk8s/docs/04-Operations/node-management-best-practices/
func formatTimeForK8sLabel(t time.Time) string {
	return t.UTC().Format("2006-01-02T15-04-05Z")
}

func applyRebootLabelsAndSetNodeCondition(ctx workflow.Context, input RebootNodeInput, labels ...string) error {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: 0, // Retry indefinitely
		},
	}

	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.LabelNode,
		activities.LabelNodeInput{
			NodeID:                        input.NodeID,
			Labels:                        labels,
			NodeRebootingConditionMessage: "Labeling node",
		}).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to apply label to node: %w", err)
	}

	return nil
}

func getNode(ctx workflow.Context, nodeID uuid.UUID) (corev1.Node, error) {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: 0, // Retry indefinitely
		},
	}

	var result activities.GetKubeNodeResult
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.GetKubeNode,
		activities.GetKubeNodeInput{
			NodeID:                        nodeID,
			NodeRebootingConditionMessage: "Getting node info",
		}).Get(ctx, &result); err != nil {
		return corev1.Node{}, fmt.Errorf("failed to get node bootID: %w", err)
	}

	return result.Node, nil
}

func rebootNode(ctx workflow.Context, nodeCtx activities.NodeProvisioningContext, rebootAttempt int, reason string) (time.Time, error) {
	switch nodeCtx.InfraMode {
	case activities.InfraModeCAPI:
		return rebootCAPINode(ctx, nodeCtx.NodeID, rebootAttempt, reason)
	case activities.InfraModeForge:
		return rebootForgeNode(ctx, nodeCtx.NodeID, rebootAttempt)
	default:
		return time.Time{}, fmt.Errorf("unsupported node infra mode %q", nodeCtx.InfraMode)
	}
}

func rebootCAPINode(ctx workflow.Context, nodeID uuid.UUID, rebootAttempt int, reason string) (time.Time, error) {
	message := fmt.Sprintf("Rebooting node (attempt: %d): %s", rebootAttempt, reason)

	// Set the rebooting node condition in CWE db
	if err := updateNodeRebootingCondition(ctx, nodeID, db.ConditionStatusTrue, message); err != nil {
		return time.Time{}, fmt.Errorf("failed to update node conditions: %w", err)
	}

	return applyCAPIMachineRebootAnnotation(ctx, nodeID)
}

// rebootForgeNode tries the in-guest reboot runbook first; if that fails, falls back to Forge API.
func rebootForgeNode(ctx workflow.Context, nodeID uuid.UUID, rebootAttempt int) (time.Time, error) {
	logger := workflow.GetLogger(ctx)
	rebootStartedTime := workflow.Now(ctx).UTC()

	err := softRebootNode(ctx, nodeID, rebootAttempt, 1)
	if err == nil {
		logger.Info("Soft reboot initiated", "nodeID", nodeID, "rebootAttempt", rebootAttempt)
		return rebootStartedTime, nil
	}

	if errors.Is(ctx.Err(), workflow.ErrCanceled) || temporal.IsCanceledError(err) {
		logger.Info("Reboot workflow canceled", "nodeID", nodeID, "rebootAttempt", rebootAttempt)
		return time.Time{}, err
	}

	logger.Warn("Soft reboot failed, falling back to Forge hard reboot", "nodeID", nodeID, "rebootAttempt", rebootAttempt, "error", err)
	return forgeHardRebootNode(ctx, nodeID, rebootAttempt)
}

func forgeHardRebootNode(ctx workflow.Context, nodeID uuid.UUID, rebootAttempt int) (time.Time, error) {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: 0, // Retry indefinitely
		},
	}

	var result activities.ForgeRebootNodeResult
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.ForgeRebootNode,
		activities.ForgeRebootNodeInput{
			NodeID:                        nodeID,
			NodeRebootingConditionMessage: fmt.Sprintf("Hard rebooting node via Forge (attempt: %d)", rebootAttempt),
		}).Get(ctx, &result); err != nil {
		return time.Time{}, fmt.Errorf("failed to forge reboot node: %w", err)
	}

	return result.RebootStartedTime, nil
}

func waitForNodeReboot(ctx workflow.Context, input RebootNodeInput, rebootAttempt int, previousBootID string, annotationAfterTime time.Time) error {
	var a *activities.Activities

	// Linear timeout increase based on reboot attempt
	// e.g. 30m, 45m, 60m, ...
	timeout := min(input.InitialTimeout+(input.TimeoutIncrement*time.Duration(rebootAttempt-1)), input.MaxTimeout)

	// We have observed on a few occasions that it takes multiple reboots until the instance is successfully rebooted.
	// Failing the workflow on timeout allows the workflow loop to control the workflow retries.
	// The workflow loop will retry both the forge reboot and the wait with the increasing timeout.
	// This also allows an SRE to remove the reboot label from the node if desired, or to try indefinitely.
	activityOptions := workflow.ActivityOptions{
		ScheduleToCloseTimeout: timeout,
		HeartbeatTimeout:       2 * time.Minute,
	}

	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.WaitForNodeReboot,
		activities.WaitForNodeRebootInput{
			NodeID:                        input.NodeID,
			PreviousBootID:                previousBootID,
			CheckRebootCompleteAnnotation: true,
			AnnotationAfterTime:           annotationAfterTime,
			PollingInterval:               input.PollingInterval,
		}).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to wait for node reboot: %w", err)
	}

	return nil
}

func applyCAPIMachineRebootAnnotation(ctx workflow.Context, nodeID uuid.UUID) (time.Time, error) {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: 0, // Retry indefinitely
		},
	}

	var result activities.ApplyCAPIMachineRebootAnnotationResult
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.ApplyCAPIMachineRebootAnnotation,
		activities.ApplyCAPIMachineRebootAnnotationInput{
			NodeID: nodeID,
		}).Get(ctx, &result); err != nil {
		return time.Time{}, fmt.Errorf("failed to apply capi machine reboot annotation: %w", err)
	}

	return result.RebootStartedTime, nil
}

func deleteCAPIMachineRebootAnnotation(ctx workflow.Context, nodeID uuid.UUID) error {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: 0, // Retry indefinitely
		},
	}

	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.DeleteCAPIMachineRebootAnnotation,
		activities.DeleteCAPIMachineRebootAnnotationInput{
			NodeID: nodeID,
		}).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to delete capi machine reboot annotation: %w", err)
	}

	return nil
}

func updateNodeRebootingCondition(ctx workflow.Context, nodeID uuid.UUID, status db.ConditionStatus, message string) error {
	var a *activities.Activities

	return workflow.ExecuteActivity(withDBOptions(ctx), a.UpdateNodeConditions, activities.NodeConditionsUpdate{
		NodeID: nodeID,
		Conditions: activities.NodeConditions{
			Rebooting: &activities.ConditionUpdate{
				Status:  status,
				Message: message,
			},
		},
	}).Get(ctx, nil)
}

func isControlPlaneNode(ctx workflow.Context, nodeID uuid.UUID) (bool, error) {
	logger := workflow.GetLogger(ctx)
	var a *activities.Activities

	// Get nodePool Info using nodeID
	nodePool := db.NodePool{}
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.GetNodePoolForNode, nodeID).Get(ctx, &nodePool); err != nil {
		return false, fmt.Errorf("failed to get nodepool info for node: %w", err)
	}

	// check if node belongs to controlplane nodepool
	if nodePool.ControlPlane {
		logger.Debug("Node belongs to control-plane nodepool", "nodeID", nodeID, "nodePoolName", nodePool.Name)
		return true, nil
	}
	return false, nil
}
