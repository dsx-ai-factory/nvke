// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/workflows"
)

type CertRenewalController struct {
	cfg               config.Config
	db                db.DB
	logger            *slog.Logger
	cweTemporalClient workflowStarter
}

type workflowStarter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
}

func NewCertRenewalController(cfg config.Config, logger *slog.Logger, dbInstance db.DB, cweTemporalClient workflowStarter) *CertRenewalController {
	return &CertRenewalController{
		cfg:               cfg,
		db:                dbInstance,
		logger:            logger,
		cweTemporalClient: cweTemporalClient,
	}
}

func (c *CertRenewalController) Reconcile(ctx context.Context, site db.Site) error {
	if !c.cfg.CertRenewalControllerEnabled {
		c.logger.Debug("cert renewal controller is disabled", "siteID", site.ID)
		return nil
	}
	if c.cweTemporalClient == nil {
		return fmt.Errorf("CWE Temporal client is required when cert renewal controller is enabled")
	}

	now := time.Now()
	if !maintenanceDue(site.NextCertRenewalDue, now) {
		return nil
	}

	clusters, err := c.db.ListClusters(ctx, db.ClusterFilters{SiteID: &site.ID})
	if err != nil {
		return fmt.Errorf("failed to list clusters: %w", err)
	}

	for _, cluster := range clusters {
		if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
			continue
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
			continue
		}
	}

	nextDue := now.Add(c.cfg.CertRenewalInterval)
	if err := c.db.UpdateSiteNextCertRenewalDue(ctx, site.ID, nextDue); err != nil {
		c.logger.Error("Failed to update cert renewal due time", "error", err, "siteID", site.ID)
	}

	return nil
}

func maintenanceDue(nextDue *time.Time, now time.Time) bool {
	return nextDue == nil || !nextDue.After(now)
}
