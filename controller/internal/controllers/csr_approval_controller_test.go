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

func TestCSRApprovalControllerReconcile(t *testing.T) {
	t.Run("skips when disabled", func(t *testing.T) {
		mockDB := &testutil.MockDB{}
		controller := NewCSRApprovalController(config.Config{}, slog.Default(), mockDB, nil)

		require.NoError(t, controller.Reconcile(context.Background(), db.Site{ID: uuid.New()}))
		mockDB.AssertNotCalled(t, "ListClusters", mock.Anything, mock.Anything)
	})

	t.Run("starts workflow and advances due time", func(t *testing.T) {
		ctx := context.Background()
		siteID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
		clusterID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
		due := time.Now().Add(-time.Minute)
		interval := 10 * time.Minute

		mockDB := &testutil.MockDB{}
		mockDB.On("ListClusters", mock.Anything, db.ClusterFilters{SiteID: &siteID}).Return([]db.Cluster{{
			ID:     clusterID,
			SiteID: siteID,
		}}, nil).Once()
		mockDB.On("UpdateSiteNextCSRApprovalDue", mock.Anything, siteID, mock.MatchedBy(func(next time.Time) bool {
			return next.After(time.Now())
		})).Return(nil).Once()

		temporalClient := &mockWorkflowStarter{}
		temporalClient.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(opts client.StartWorkflowOptions) bool {
			return opts.TaskQueue == "test-task-queue" &&
				opts.ID == constants.ApproveNodeCSRsWorkflowPrefix+clusterID.String() &&
				opts.WorkflowIDConflictPolicy == enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING &&
				opts.WorkflowIDReusePolicy == enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE
		}), mock.Anything, mock.MatchedBy(func(input workflows.ApproveNodeCSRsInput) bool {
			return input.ClusterID == clusterID
		})).Return(mockWorkflowRun{id: "run"}, nil).Once()

		controller := NewCSRApprovalController(config.Config{
			TemporalTaskQueue:            "test-task-queue",
			CSRApprovalControllerEnabled: true,
			CSRApprovalInterval:          interval,
		}, slog.Default(), mockDB, temporalClient)

		require.NoError(t, controller.Reconcile(ctx, db.Site{ID: siteID, NextCSRApprovalDue: &due}))
		mockDB.AssertExpectations(t)
		temporalClient.AssertExpectations(t)
	})
}

type mockWorkflowStarter struct {
	mock.Mock
}

func (m *mockWorkflowStarter) ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error) {
	callArgs := make([]interface{}, 0, 3+len(args))
	callArgs = append(callArgs, ctx, options, workflow)
	callArgs = append(callArgs, args...)
	ret := m.Called(callArgs...)
	run, _ := ret.Get(0).(client.WorkflowRun)
	return run, ret.Error(1)
}

type mockWorkflowRun struct {
	id    string
	runID string
}

func (m mockWorkflowRun) GetID() string {
	return m.id
}

func (m mockWorkflowRun) GetRunID() string {
	return m.runID
}

func (m mockWorkflowRun) Get(_ context.Context, _ interface{}) error {
	return nil
}
