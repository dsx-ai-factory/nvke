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

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile/filters"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/temporal/workflows"
)

type Controller struct {
	cfg               config.Config
	db                *dbwrapper.DB
	logger            *slog.Logger
	cweTemporalClient workflowStarter
}

type workflowStarter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
}

func NewController(cfg config.Config, logger *slog.Logger, dbInstance *dbwrapper.DB, cweTemporalClient workflowStarter) *Controller {
	return &Controller{
		cfg:               cfg,
		db:                dbInstance,
		logger:            logger,
		cweTemporalClient: cweTemporalClient,
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
	if !maintenanceDue(cluster.NextCertRenewalDue, now) {
		return reconcile.Result{RequeueAfter: cluster.NextCertRenewalDue.Sub(now)}, nil
	}

	workflowID := constants.RenewK8sCertsWorkflowPrefix + cluster.ID.String()
	if _, err := c.cweTemporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		TaskQueue:                c.cfg.TemporalTaskQueue,
		ID:                       workflowID,
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, workflows.RenewK8sCerts, workflows.RenewK8sCertsInput{
		ClusterID:     cluster.ID,
		ThresholdDays: c.cfg.CertRenewalThresholdDays,
	}); err != nil {
		c.logger.Error("Failed to start cert renewal workflow", "error", err, "clusterID", cluster.ID, "workflowID", workflowID)
		return reconcile.Result{}, fmt.Errorf("start cert renewal workflow for cluster %s: %w", cluster.ID, err)
	}

	nextDue := now.Add(c.cfg.CertRenewalInterval)
	if err := c.db.UpdateClusterNextCertRenewalDue(ctx, cluster.ID, nextDue); err != nil {
		return reconcile.Result{}, fmt.Errorf("update cert renewal due time for cluster %s: %w", cluster.ID, err)
	}

	return reconcile.Result{RequeueAfter: c.cfg.CertRenewalInterval}, nil
}

func maintenanceDue(nextDue *time.Time, now time.Time) bool {
	return nextDue == nil || !nextDue.After(now)
}
