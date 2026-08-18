// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/workflows"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestCertRenewalControllerReconcile(t *testing.T) {
	t.Run("starts workflow and advances due time", func(t *testing.T) {
		ctx := context.Background()
		siteID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
		clusterID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
		due := time.Now().Add(-time.Minute)
		interval := 12 * time.Hour

		mockDB := &testutil.MockDB{}
		mockDB.On("ListClusters", mock.Anything, db.ClusterFilters{SiteID: &siteID}).Return([]db.Cluster{{
			ID:     clusterID,
			SiteID: siteID,
		}}, nil).Once()
		mockDB.On("UpdateSiteNextCertRenewalDue", mock.Anything, siteID, mock.MatchedBy(func(next time.Time) bool {
			return next.After(time.Now())
		})).Return(nil).Once()

		temporalClient := &mockWorkflowStarter{}
		temporalClient.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(opts client.StartWorkflowOptions) bool {
			return opts.TaskQueue == "test-task-queue" &&
				opts.ID == constants.RenewK8sCertsWorkflowPrefix+clusterID.String() &&
				opts.WorkflowIDConflictPolicy == enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING &&
				opts.WorkflowIDReusePolicy == enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE
		}), mock.Anything, mock.MatchedBy(func(input workflows.RenewK8sCertsInput) bool {
			return input.ClusterID == clusterID && input.ThresholdDays == 14
		})).Return(mockWorkflowRun{id: "run"}, nil).Once()

		controller := NewCertRenewalController(config.Config{
			TemporalTaskQueue:            "test-task-queue",
			CertRenewalControllerEnabled: true,
			CertRenewalInterval:          interval,
			CertRenewalThresholdDays:     14,
		}, slog.Default(), mockDB, temporalClient)

		require.NoError(t, controller.Reconcile(ctx, db.Site{ID: siteID, NextCertRenewalDue: &due}))
		mockDB.AssertExpectations(t)
		temporalClient.AssertExpectations(t)
	})

	t.Run("skips site not yet due", func(t *testing.T) {
		ctx := context.Background()
		siteID := uuid.New()
		nextDue := time.Now().Add(time.Hour)
		mockDB := &testutil.MockDB{}

		temporalClient := &mockWorkflowStarter{}
		controller := NewCertRenewalController(config.Config{
			CertRenewalControllerEnabled: true,
			CertRenewalInterval:          time.Hour,
			CertRenewalThresholdDays:     7,
		}, slog.Default(), mockDB, temporalClient)

		require.NoError(t, controller.Reconcile(ctx, db.Site{ID: siteID, NextCertRenewalDue: &nextDue}))
		mockDB.AssertExpectations(t)
		mockDB.AssertNotCalled(t, "ListClusters", mock.Anything, mock.Anything)
		temporalClient.AssertNotCalled(t, "ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})
}
