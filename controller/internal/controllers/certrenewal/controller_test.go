// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certrenewal

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/workflows"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

const certRenewalInterval = 12 * time.Hour

func TestUnit_ControllerReconcile(t *testing.T) {
	t.Run("schedules the first check without a workflow", func(t *testing.T) {
		ctx := context.Background()
		clusterID := uuid.New()
		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID}, nil).Once()
		mockDB.On("UpdateClusterNextCertRenewalDue", mock.Anything, clusterID, mock.MatchedBy(func(next time.Time) bool {
			return next.After(time.Now().Add(certRenewalInterval - time.Minute))
		})).Return(nil).Once()

		temporalClient := &mockWorkflowStarter{}
		controller := newController(mockDB, temporalClient)
		result, err := controller.Reconcile(ctx, dbreconcile.ClusterKey(clusterID))

		require.NoError(t, err)
		require.Equal(t, reconcile.Result{RequeueAfter: certRenewalInterval}, result)
		mockDB.AssertExpectations(t)
		temporalClient.AssertNotCalled(t, "ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("starts workflow and advances due time", func(t *testing.T) {
		ctx := context.Background()
		clusterID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
		due := time.Now().Add(-time.Minute)
		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
			ID:                 clusterID,
			NextCertRenewalDue: &due,
		}, nil).Once()
		mockDB.On("UpdateClusterNextCertRenewalDue", mock.Anything, clusterID, mock.MatchedBy(func(next time.Time) bool {
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

		controller := newController(mockDB, temporalClient)
		result, err := controller.Reconcile(ctx, dbreconcile.ClusterKey(clusterID))

		require.NoError(t, err)
		require.Equal(t, reconcile.Result{RequeueAfter: certRenewalInterval}, result)
		mockDB.AssertExpectations(t)
		temporalClient.AssertExpectations(t)
	})

	t.Run("skips cluster not yet due", func(t *testing.T) {
		ctx := context.Background()
		clusterID := uuid.New()
		nextDue := time.Now().Add(time.Hour)
		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
			ID:                 clusterID,
			NextCertRenewalDue: &nextDue,
		}, nil).Once()

		temporalClient := &mockWorkflowStarter{}
		controller := newController(mockDB, temporalClient)
		result, err := controller.Reconcile(ctx, dbreconcile.ClusterKey(clusterID))

		require.NoError(t, err)
		require.Greater(t, result.RequeueAfter, time.Duration(0))
		require.LessOrEqual(t, result.RequeueAfter, time.Hour)
		mockDB.AssertExpectations(t)
		temporalClient.AssertNotCalled(t, "ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("does not advance due time when workflow start fails", func(t *testing.T) {
		ctx := context.Background()
		clusterID := uuid.New()
		due := time.Now().Add(-time.Minute)
		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, NextCertRenewalDue: &due}, nil).Once()

		temporalClient := &mockWorkflowStarter{}
		temporalClient.On("ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(nil, errors.New("temporal unavailable")).Once()

		controller := newController(mockDB, temporalClient)
		result, err := controller.Reconcile(ctx, dbreconcile.ClusterKey(clusterID))

		require.ErrorContains(t, err, "start cert renewal workflow")
		require.Equal(t, reconcile.Result{}, result)
		mockDB.AssertNotCalled(t, "UpdateClusterNextCertRenewalDue", mock.Anything, mock.Anything, mock.Anything)
		mockDB.AssertExpectations(t)
		temporalClient.AssertExpectations(t)
	})

	t.Run("skips deleting cluster", func(t *testing.T) {
		ctx := context.Background()
		clusterID := uuid.New()
		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
			ID: clusterID,
			Conditions: db.ClusterConditions{
				Deleting: db.Condition{Status: db.ConditionStatusTrue},
			},
		}, nil).Once()

		temporalClient := &mockWorkflowStarter{}
		controller := newController(mockDB, temporalClient)
		result, err := controller.Reconcile(ctx, dbreconcile.ClusterKey(clusterID))

		require.NoError(t, err)
		require.Equal(t, reconcile.Result{}, result)
		mockDB.AssertExpectations(t)
		temporalClient.AssertNotCalled(t, "ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("skips missing cluster", func(t *testing.T) {
		ctx := context.Background()
		clusterID := uuid.New()
		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{}, db.ErrNotFound).Once()

		temporalClient := &mockWorkflowStarter{}
		controller := newController(mockDB, temporalClient)
		result, err := controller.Reconcile(ctx, dbreconcile.ClusterKey(clusterID))

		require.NoError(t, err)
		require.Equal(t, reconcile.Result{}, result)
		mockDB.AssertExpectations(t)
		temporalClient.AssertNotCalled(t, "ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})
}

func newController(mockDB *testutil.MockDB, temporalClient workflowStarter) *Controller {
	return NewController(config.Config{
		TemporalTaskQueue:        "test-task-queue",
		CertRenewalInterval:      certRenewalInterval,
		CertRenewalThresholdDays: 14,
	}, slog.Default(), dbwrapper.New(mockDB, slog.Default()), temporalClient)
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
