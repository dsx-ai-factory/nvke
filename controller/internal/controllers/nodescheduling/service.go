// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nodescheduling contains shared node provisioning scheduler helpers.
package nodescheduling

import (
	"context"
	"fmt"
	"log/slog"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/otelx"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/workflows"
)

// Service owns shared node scheduling side effects.
type Service struct {
	cfg                      config.Config
	db                       db.DB
	logger                   *slog.Logger
	controllerTemporalClient WorkflowStarter
}

// WorkflowStarter is the Temporal client behavior needed by node scheduling.
type WorkflowStarter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
}

// NewService creates a Service.
func NewService(
	cfg config.Config,
	logger *slog.Logger,
	dbInstance db.DB,
	controllerTemporalClient WorkflowStarter,
) *Service {
	return &Service{
		cfg:                      cfg,
		db:                       dbInstance,
		logger:                   logger,
		controllerTemporalClient: controllerTemporalClient,
	}
}

// ScheduleCAPIInfraNodes starts ProvisionNode workflows without Forge
// preflight checks.
func (s *Service) ScheduleCAPIInfraNodes(ctx context.Context, _ *slog.Logger, pendingNodes []db.Node) (reconcile.Result, error) {
	for _, pendingNode := range pendingNodes {
		if err := s.startProvisionNodeWorkflow(ctx, pendingNode); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}

func (s *Service) startProvisionNodeWorkflow(ctx context.Context, pendingNode db.Node) error {
	workflowID := constants.ProvisionNodeWorkflowPrefix + pendingNode.ID.String()
	workflowOpts := client.StartWorkflowOptions{
		TaskQueue:                s.cfg.TemporalTaskQueue,
		ID:                       workflowID,
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
	}
	ctx, span := otelx.StartWorkflowSpan(ctx, otelx.WorkflowStart, otelx.WorkflowAttributes{
		WorkflowType: workflows.ProvisionNodeWorkflowType,
		WorkflowID:   workflowID,
		ClusterID:    pendingNode.ClusterID,
		NodePoolID:   pendingNode.NodePoolID,
		NodeID:       pendingNode.ID,
		NodeName:     pendingNode.Name,
	})
	run, err := s.controllerTemporalClient.ExecuteWorkflow(ctx, workflowOpts, workflows.ProvisionNode, workflows.ProvisionNodeInput{
		NodeID:                         pendingNode.ID,
		RebootNodeWaitForUptimeTimeout: s.cfg.NodeRebootMaxTimeout,
		// Control-plane nodes defer join to the control-plane controller, which
		// serializes joins one node at a time.
		SkipJoin: pendingNode.ControlPlane,
	})
	otelx.EndWorkflowSpan(span, err)
	if err != nil {
		return fmt.Errorf("starting ProvisionNode workflow for node %s: %w", pendingNode.ID, err)
	}

	logArgs := []any{
		"cluster_id", pendingNode.ClusterID,
	}
	if pendingNode.IsManagedByNodePool() {
		logArgs = append(logArgs, "node_pool_id", pendingNode.NodePoolID)
	}
	logArgs = append(logArgs,
		"node_id", pendingNode.ID,
		"node_name", pendingNode.Name,
		"workflow_id", workflowID,
		"run_id", run.GetRunID(),
	)
	s.logger.InfoContext(ctx, "Started (or already running) ProvisionNode workflow", logArgs...)
	return nil
}
