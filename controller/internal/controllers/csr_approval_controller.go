// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

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

type CSRApprovalController struct {
	cfg               config.Config
	db                db.DB
	logger            *slog.Logger
	cweTemporalClient workflowStarter
}

type workflowStarter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
}

func NewCSRApprovalController(cfg config.Config, logger *slog.Logger, dbInstance db.DB, cweTemporalClient workflowStarter) *CSRApprovalController {
	return &CSRApprovalController{
		cfg:               cfg,
		db:                dbInstance,
		logger:            logger,
		cweTemporalClient: cweTemporalClient,
	}
}

func (c *CSRApprovalController) Reconcile(ctx context.Context, site db.Site) error {
	if !c.cfg.CSRApprovalControllerEnabled {
		c.logger.Debug("CSR approval controller is disabled", "siteID", site.ID)
		return nil
	}
	if c.cweTemporalClient == nil {
		return fmt.Errorf("CWE Temporal client is required when CSR approval controller is enabled")
	}

	now := time.Now()
	if !maintenanceDue(site.NextCSRApprovalDue, now) {
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

		workflowID := constants.ApproveNodeCSRsWorkflowPrefix + cluster.ID.String()
		if _, err := c.cweTemporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			TaskQueue:                c.cfg.TemporalTaskQueue,
			ID:                       workflowID,
			WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
			WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		}, workflows.ApproveNodeCSRs, workflows.ApproveNodeCSRsInput{
			ClusterID: cluster.ID,
		}); err != nil {
			c.logger.Error("Failed to start CSR approval workflow", "error", err, "clusterID", cluster.ID, "workflowID", workflowID)
			continue
		}
	}

	nextDue := now.Add(c.cfg.CSRApprovalInterval)
	if err := c.db.UpdateSiteNextCSRApprovalDue(ctx, site.ID, nextDue); err != nil {
		c.logger.Error("Failed to update CSR approval due time", "error", err, "siteID", site.ID)
	}

	return nil
}

func maintenanceDue(nextDue *time.Time, now time.Time) bool {
	return nextDue == nil || !nextDue.After(now)
}
