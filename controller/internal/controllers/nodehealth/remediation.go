// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package nodehealth

import (
	"context"
	"fmt"
	"strings"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/workflows"
)

func (c *Controller) startNodeRemediationWorkflowsIfNecessary(ctx context.Context, node db.Node, kubeNode *corev1.Node) error {
	labels := kubeNode.GetLabels()

	// Prefer recycle over reboot if both labels are present
	if reason, terminateLabelPresent := labels[constants.LabelKeyNodeLabelTerminate]; terminateLabelPresent {
		return c.startTeardownNodeWorkflow(ctx, node, kubeNode, reason)
	}

	if reason, rebootLabelPresent := labels[constants.LabelKeyNodeLabelReboot]; rebootLabelPresent {
		return c.startNodeRebootWorkflow(ctx, node, kubeNode, reason)
	}

	c.logger.Debug("No node remediation workflows to start for node", "nodeID", node.ID.String(), "nodeName", node.Name, "nvidiaLabels", nvidiaLabels(labels))
	return nil
}

func (c *Controller) startTeardownNodeWorkflow(ctx context.Context, node db.Node, kubeNode *corev1.Node, reason string) error {
	workflowID := constants.TeardownNodeWorkflowPrefix + node.ID.String()
	options := client.StartWorkflowOptions{
		ID:                       workflowID,
		TaskQueue:                c.cfg.TemporalTaskQueue,
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}

	var machineHealthIssue *workflows.MachineHealthIssue
	if annotation, ok := kubeNode.GetAnnotations()[constants.NodeAnnotationKeyMachineHealthIssue]; ok {
		var err error
		machineHealthIssue, err = workflows.NewMachineHealthIssueFromJSON([]byte(annotation))
		if err != nil {
			return fmt.Errorf("failed to parse machine health issue annotation: %w", err)
		}
		if machineHealthIssue != nil {
			c.logger.Info("Machine health issue reported", "annotation", annotation, "machineHealthIssue", machineHealthIssue)
		}
	}

	input := workflows.TeardownNodeInput{
		NodeID:                node.ID,
		Reason:                reason,
		SkipRemoveFromCluster: false,
		MachineHealthIssue:    machineHealthIssue,
	}

	run, err := c.cweTemporalClient.ExecuteWorkflow(ctx, options, workflows.TeardownNode, input)
	if err != nil {
		return fmt.Errorf("failed to execute teardown node workflow for node %s: %w", node.ID, err)
	}

	c.logger.Info("Started (or already running) teardown node workflow",
		"workflowID", run.GetID(),
		"runID", run.GetRunID(),
		"nodeID", node.ID.String(),
		"nodeName", node.Name,
		"nvidiaLabels", nvidiaLabels(kubeNode.GetLabels()))
	return nil
}

func (c *Controller) startNodeRebootWorkflow(ctx context.Context, node db.Node, kubeNode *corev1.Node, reason string) error {
	workflowID := constants.RebootNodeWorkflowPrefix + node.ID.String()
	options := client.StartWorkflowOptions{
		ID:                       workflowID,
		TaskQueue:                c.cfg.TemporalTaskQueue,
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}

	input := workflows.RebootNodeInput{
		NodeID:           node.ID,
		Reason:           reason,
		InitialTimeout:   c.cfg.NodeRebootInitialTimeout,
		TimeoutIncrement: c.cfg.NodeRebootTimeoutIncrement,
		MaxTimeout:       c.cfg.NodeRebootMaxTimeout,
		PollingInterval:  c.cfg.NodeRebootPollingInterval,
	}

	run, err := c.cweTemporalClient.ExecuteWorkflow(ctx, options, workflows.RebootNode, input)
	if err != nil {
		return fmt.Errorf("failed to execute reboot node workflow for node %s: %w", node.ID, err)
	}

	c.logger.Info("Started (or already running) reboot node workflow",
		"workflowID", run.GetID(),
		"runID", run.GetRunID(),
		"nodeID", node.ID.String(),
		"nodeName", node.Name,
		"nvidiaLabels", nvidiaLabels(kubeNode.GetLabels()))
	return nil
}

func nvidiaLabels(labels map[string]string) map[string]string {
	filteredLabels := make(map[string]string)
	for key, value := range labels {
		if strings.Contains(key, "nvidia") {
			filteredLabels[key] = value
		}
	}

	return filteredLabels
}
