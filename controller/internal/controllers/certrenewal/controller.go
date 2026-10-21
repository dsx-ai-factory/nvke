// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certrenewal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/filters"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/otelx"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/workflows"
)

type Controller struct {
	cfg                      config.Config
	db                       *dbwrapper.DB
	logger                   *slog.Logger
	controllerTemporalClient workflowStarter
}

type workflowStarter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
}

func NewController(cfg config.Config, logger *slog.Logger, dbInstance *dbwrapper.DB, controllerTemporalClient workflowStarter) *Controller {
	return &Controller{
		cfg:                      cfg,
		db:                       dbInstance,
		logger:                   logger,
		controllerTemporalClient: controllerTemporalClient,
	}
}

func (c *Controller) SetupWithManager(mgr manager.Manager) error {
	return dbreconcile.SetupController(
		mgr,
		"cert-renewal-controller",
		c,
		dbwrapper.NewClusterSource(c.db, dbfilters.Cluster{}),
	)
}

func (c *Controller) Reconcile(ctx context.Context, key dbreconcile.Key) (reconcile.Result, error) {
	cluster, err := c.db.GetCluster(ctx, key.ID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			c.logger.Debug("cluster not found, skipping cert renewal", "clusterID", key.ID.String())
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
		return reconcile.Result{}, nil
	}

	now := time.Now()
	// The first look at a cluster schedules the check instead of running it: a new cluster's certificates
	// are fresh and its nodes may still be joining, so a renewal pass there only collides with other work.
	if cluster.NextCertRenewalDue == nil {
		nextDue := now.Add(c.cfg.CertRenewalInterval)
		if err := c.db.UpdateClusterNextCertRenewalDue(ctx, cluster.ID, nextDue); err != nil {
			return reconcile.Result{}, fmt.Errorf("schedule the first cert renewal check for cluster %s: %w", cluster.ID, err)
		}
		return reconcile.Result{RequeueAfter: c.cfg.CertRenewalInterval}, nil
	}
	if !maintenanceDue(*cluster.NextCertRenewalDue, now) {
		return reconcile.Result{RequeueAfter: cluster.NextCertRenewalDue.Sub(now)}, nil
	}

	workflowID := constants.RenewK8sCertsWorkflowPrefix + cluster.ID.String()
	workflowCtx, span := otelx.StartWorkflowSpan(ctx, otelx.WorkflowStart, otelx.WorkflowAttributes{
		WorkflowType: workflows.RenewK8sCertsWorkflowType,
		WorkflowID:   workflowID,
		ClusterID:    cluster.ID,
		ClusterName:  cluster.Name,
	})
	_, err = c.controllerTemporalClient.ExecuteWorkflow(workflowCtx, client.StartWorkflowOptions{
		TaskQueue:                c.cfg.TemporalTaskQueue,
		ID:                       workflowID,
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, workflows.RenewK8sCerts, workflows.RenewK8sCertsInput{
		ClusterID:     cluster.ID,
		ThresholdDays: c.cfg.CertRenewalThresholdDays,
	})
	otelx.EndWorkflowSpan(span, err)
	if err != nil {
		c.logger.ErrorContext(workflowCtx, "Failed to start cert renewal workflow", "error", err, "cluster_id", cluster.ID, "workflow_id", workflowID)
		return reconcile.Result{}, fmt.Errorf("start cert renewal workflow for cluster %s: %w", cluster.ID, err)
	}

	nextDue := now.Add(c.cfg.CertRenewalInterval)
	if err := c.db.UpdateClusterNextCertRenewalDue(ctx, cluster.ID, nextDue); err != nil {
		return reconcile.Result{}, fmt.Errorf("update cert renewal due time for cluster %s: %w", cluster.ID, err)
	}

	return reconcile.Result{RequeueAfter: c.cfg.CertRenewalInterval}, nil
}

func maintenanceDue(nextDue, now time.Time) bool {
	return !nextDue.After(now)
}
