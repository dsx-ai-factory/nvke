// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certrenewal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
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

// Reconcile reuses a due-time-derived workflow ID so repeated observations do not
// create distinct cycles. The first look at a cluster only schedules its first
// check one interval ahead; after that, dispatch never advances the due time.
// Released failures retry after one hour; retained failures halt, and so does a
// two-voter refusal until the desired control-plane count changes. The first
// cycle also waits for the full control plane to join rather than treating
// Provisioned as cluster readiness.
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
	if cluster.Conditions.Provisioned.Status != db.ConditionStatusTrue {
		return reconcile.Result{}, nil
	}
	workflowID := scheduledWorkflowID(cluster.ID, *cluster.NextCertRenewalDue)
	if !maintenanceDue(*cluster.NextCertRenewalDue, now) {
		return reconcile.Result{RequeueAfter: cluster.NextCertRenewalDue.Sub(now)}, nil
	}

	status := cluster.Conditions.CertificateRenewal
	if status != nil && status.State == db.CertificateRenewalStateFailed {
		c.logger.Error(
			"Certificate renewal failed and remains overdue",
			"clusterID", cluster.ID,
			"failedNodeID", status.FailedNodeID,
			"phase", status.Phase,
			"message", status.Message,
		)
		return reconcile.Result{RequeueAfter: 15 * time.Minute}, nil
	}
	if scheduledCycleMatches(status, cluster.NextCertRenewalDue) {
		switch status.State {
		case db.CertificateRenewalStateAccepted:
			c.logger.Info("Certificate renewal is accepted and remains overdue until completion", "clusterID", cluster.ID)
			return reconcile.Result{RequeueAfter: time.Minute}, nil
		case db.CertificateRenewalStateSucceeded, db.CertificateRenewalStateFailed:
			// Success normally advances the due time atomically, and failure was
			// handled above. An inconsistent old row should not suppress a new cycle.
		}
	}
	if update := cluster.Conditions.UpdateInProgress; update != nil {
		c.logger.Info("Deferring certificate renewal while another cluster update is in progress", "clusterID", cluster.ID, "operation", update.Type)
		return reconcile.Result{RequeueAfter: time.Minute}, nil
	}

	if status == nil {
		// Provisioned is set after the first control-plane node is initialized;
		// the remaining nodes may still be joining. Leave the workflow's health
		// recheck in place, but do not start the first cycle before they are ready.
		controlPlane := true
		nodes, err := c.db.ListNodes(ctx, db.NodeFilters{ClusterID: &cluster.ID, ControlPlane: &controlPlane})
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("list control-plane nodes for initial certificate renewal: %w", err)
		}
		if len(nodes) < cluster.ControlPlaneConfig.DesiredCount {
			c.logger.Info("Deferring initial certificate renewal until all control-plane nodes are present", "clusterID", cluster.ID)
			return reconcile.Result{RequeueAfter: time.Minute}, nil
		}
		for _, node := range nodes {
			if node.Conditions.Joined.Status != db.ConditionStatusTrue ||
				node.Conditions.KubernetesReady.Status != db.ConditionStatusTrue ||
				node.Conditions.NodeAgentReady.Status != db.ConditionStatusTrue {
				c.logger.Info("Deferring initial certificate renewal until control-plane nodes are ready", "clusterID", cluster.ID, "nodeID", node.ID)
				return reconcile.Result{RequeueAfter: time.Minute}, nil
			}
		}
	}

	workflowCtx, span := otelx.StartWorkflowSpan(ctx, otelx.WorkflowStart, otelx.WorkflowAttributes{
		WorkflowType: workflows.RenewK8sCertsV2WorkflowType,
		WorkflowID:   workflowID,
		ClusterID:    cluster.ID,
		ClusterName:  cluster.Name,
	})
	_, err = c.controllerTemporalClient.ExecuteWorkflow(workflowCtx, client.StartWorkflowOptions{
		TaskQueue:                c.cfg.TemporalTaskQueue,
		ID:                       workflowID,
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
	}, workflows.RenewK8sCertsV2, workflows.RenewK8sCertsInput{
		ClusterID:     cluster.ID,
		ThresholdDays: c.cfg.CertRenewalThresholdDays,
		Force:         false,
		ScheduledFor:  cluster.NextCertRenewalDue,
	})
	otelx.EndWorkflowSpan(span, err)
	if err != nil {
		c.logger.ErrorContext(workflowCtx, "Failed to start cert renewal workflow", "error", err, "cluster_id", cluster.ID, "workflow_id", workflowID)
		return reconcile.Result{}, fmt.Errorf("start cert renewal workflow for cluster %s: %w", cluster.ID, err)
	}

	c.logger.Info("Certificate renewal workflow accepted", "clusterID", cluster.ID, "workflowID", workflowID)
	return reconcile.Result{RequeueAfter: time.Minute}, nil
}

func maintenanceDue(nextDue, now time.Time) bool {
	return !nextDue.After(now)
}

// scheduledWorkflowID identifies the due-time cycle, not this reconcile attempt.
// Only successful workflow completion changes the due time that selects the next
// scheduled ID.
func scheduledWorkflowID(clusterID uuid.UUID, scheduledFor time.Time) string {
	return fmt.Sprintf("%s%s-%d", constants.RenewK8sCertsWorkflowPrefix, clusterID, scheduledFor.UTC().UnixNano())
}

// scheduledCycleMatches excludes forced cycles, whose nil ScheduledFor must not
// be mistaken for acceptance of the initial scheduled cycle.
func scheduledCycleMatches(status *db.CertificateRenewalStatus, scheduledFor *time.Time) bool {
	if status == nil || status.Forced {
		return false
	}
	if status.ScheduledFor == nil || scheduledFor == nil {
		return status.ScheduledFor == nil && scheduledFor == nil
	}
	return status.ScheduledFor.Equal(*scheduledFor)
}
