// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderemediation

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	corev1 "k8s.io/api/core/v1"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/otelx"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/workflows"
)

type workflowStarter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
}

type nodeRemediator struct {
	cfg                      config.Config
	logger                   *slog.Logger
	controllerTemporalClient workflowStarter
}

func newNodeRemediator(cfg config.Config, logger *slog.Logger, controllerTemporalClient workflowStarter) *nodeRemediator {
	if logger == nil {
		logger = slog.Default()
	}
	return &nodeRemediator{
		cfg:                      cfg,
		logger:                   logger,
		controllerTemporalClient: controllerTemporalClient,
	}
}

func (r *nodeRemediator) startWorkflowsIfNecessary(ctx context.Context, node db.Node, kubeNode *corev1.Node) error {
	labels := kubeNode.GetLabels()

	if reason, terminateLabelPresent := labels[constants.LabelKeyNodeLabelTerminate]; terminateLabelPresent {
		return r.startTeardownNodeWorkflow(ctx, node, kubeNode, reason)
	}
	if reason, rebootLabelPresent := labels[constants.LabelKeyNodeLabelReboot]; rebootLabelPresent {
		return r.startNodeRebootWorkflow(ctx, node, kubeNode, reason)
	}

	r.logger.Debug("No node remediation workflows to start for node", "nodeID", node.ID.String(), "nodeName", node.Name, "nvidiaLabels", nvidiaLabels(labels))
	return nil
}

func (r *nodeRemediator) startTeardownNodeWorkflow(ctx context.Context, node db.Node, kubeNode *corev1.Node, reason string) error {
	workflowID := constants.TeardownNodeWorkflowPrefix + node.ID.String()
	options := client.StartWorkflowOptions{
		ID:                       workflowID,
		TaskQueue:                r.cfg.TemporalTaskQueue,
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
			r.logger.Info("Machine health issue reported", "annotation", annotation, "machineHealthIssue", machineHealthIssue)
		}
	}

	input := workflows.TeardownNodeInput{
		NodeID:                node.ID,
		Reason:                reason,
		SkipRemoveFromCluster: false,
		MachineHealthIssue:    machineHealthIssue,
	}

	ctx, span := otelx.StartWorkflowSpan(ctx, otelx.WorkflowStart, otelx.WorkflowAttributes{
		WorkflowType: constants.TeardownNodeWorkflowType,
		WorkflowID:   workflowID,
		ClusterID:    node.ClusterID,
		NodePoolID:   node.NodePoolID,
		NodeID:       node.ID,
		NodeName:     node.Name,
	})
	run, err := r.controllerTemporalClient.ExecuteWorkflow(ctx, options, workflows.TeardownNode, input)
	otelx.EndWorkflowSpan(span, err)
	if err != nil {
		return fmt.Errorf("failed to execute teardown node workflow for node %s: %w", node.ID, err)
	}

	r.logger.InfoContext(ctx, "Started (or already running) teardown node workflow",
		"workflow_id", workflowID,
		"run_id", run.GetRunID(),
		"node_id", node.ID.String(),
		"node_name", node.Name,
		"nvidia_labels", nvidiaLabels(kubeNode.GetLabels()))
	return nil
}

func (r *nodeRemediator) startNodeRebootWorkflow(ctx context.Context, node db.Node, kubeNode *corev1.Node, reason string) error {
	workflowID := constants.RebootNodeWorkflowPrefix + node.ID.String()
	options := client.StartWorkflowOptions{
		ID:                       workflowID,
		TaskQueue:                r.cfg.TemporalTaskQueue,
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}

	input := workflows.RebootNodeInput{
		NodeID:            node.ID,
		Reason:            reason,
		InitialTimeout:    r.cfg.NodeRebootInitialTimeout,
		TimeoutIncrement:  r.cfg.NodeRebootTimeoutIncrement,
		MaxTimeout:        r.cfg.NodeRebootMaxTimeout,
		PollingInterval:   r.cfg.NodeRebootPollingInterval,
		SoftRebootEnabled: r.cfg.NodeRebootSoftRebootEnabled,
	}

	ctx, span := otelx.StartWorkflowSpan(ctx, otelx.WorkflowStart, otelx.WorkflowAttributes{
		WorkflowType: workflows.RebootNodeWorkflowType,
		WorkflowID:   workflowID,
		ClusterID:    node.ClusterID,
		NodePoolID:   node.NodePoolID,
		NodeID:       node.ID,
		NodeName:     node.Name,
	})
	run, err := r.controllerTemporalClient.ExecuteWorkflow(ctx, options, workflows.RebootNode, input)
	otelx.EndWorkflowSpan(span, err)
	if err != nil {
		return fmt.Errorf("failed to execute reboot node workflow for node %s: %w", node.ID, err)
	}

	r.logger.InfoContext(ctx, "Started (or already running) reboot node workflow",
		"workflow_id", workflowID,
		"run_id", run.GetRunID(),
		"node_id", node.ID.String(),
		"node_name", node.Name,
		"nvidia_labels", nvidiaLabels(kubeNode.GetLabels()))
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
