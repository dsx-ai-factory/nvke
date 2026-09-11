// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplanenodescheduler

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/workflows"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestUnit_ClusterKeysForManagedControlPlaneNode(t *testing.T) {
	ctx := context.Background()
	clusterID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	nodePoolID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	keys, err := clusterKeysForManagedControlPlaneNode(ctx, db.Node{ClusterID: clusterID, ControlPlane: true})
	require.NoError(t, err)
	require.Equal(t,
		[]dbreconcile.Key{dbreconcile.ClusterKey(clusterID)},
		keys,
	)
	keys, err = clusterKeysForManagedControlPlaneNode(ctx, db.Node{ClusterID: clusterID})
	require.NoError(t, err)
	require.Nil(t, keys)
	keys, err = clusterKeysForManagedControlPlaneNode(ctx, db.Node{ClusterID: clusterID, ControlPlane: true, NodePoolID: nodePoolID})
	require.NoError(t, err)
	require.Nil(t, keys)
	keys, err = clusterKeysForManagedControlPlaneNode(ctx, db.Node{ControlPlane: true})
	require.NoError(t, err)
	require.Nil(t, keys)
}

func TestUnit_ControlPlaneNodeSchedulerReconcileCAPIStartsManagedPendingNodeWorkflows(t *testing.T) {
	ctx := context.Background()
	clusterID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	managedNodeID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	legacyNodePoolID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	legacyNodeID := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	pending := true
	deleting := false
	controlPlane := true

	database := &testutil.MockDB{}
	database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
		ID:           clusterID,
		ProviderType: db.ProviderTypeCAPI,
	}, nil).Once()
	database.On("ListNodes", mock.Anything, db.NodeFilters{
		ClusterID:    &clusterID,
		ControlPlane: &controlPlane,
		Pending:      &pending,
		Deleting:     &deleting,
	}).Return([]db.Node{
		{ID: managedNodeID, ClusterID: clusterID, ControlPlane: true},
		{ID: legacyNodeID, ClusterID: clusterID, ControlPlane: true, NodePoolID: legacyNodePoolID},
	}, nil).Once()

	temporalClient := &mockWorkflowStarter{}
	expectProvisionWorkflow(t, temporalClient, managedNodeID)

	scheduler := NewControlPlaneNodeScheduler(config.Config{
		TemporalTaskQueue:    "test-task-queue",
		NodeRebootMaxTimeout: 5 * time.Minute,
	}, slog.Default(), dbwrapper.New(database, slog.Default()), temporalClient)

	result, err := scheduler.Reconcile(ctx, dbreconcile.ClusterKey(clusterID))
	require.NoError(t, err)
	require.Zero(t, result)
	database.AssertExpectations(t)
	temporalClient.AssertExpectations(t)
}

func TestUnit_ControlPlaneNodeSchedulerSkipsDeletingCluster(t *testing.T) {
	ctx := context.Background()
	clusterID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	database := &testutil.MockDB{}
	database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
		ID: clusterID,
		Conditions: db.ClusterConditions{
			Deleting: db.Condition{Status: db.ConditionStatusTrue},
		},
	}, nil).Once()

	scheduler := NewControlPlaneNodeScheduler(config.Config{}, slog.Default(), dbwrapper.New(database, slog.Default()), nil)
	result, err := scheduler.Reconcile(ctx, dbreconcile.ClusterKey(clusterID))
	require.NoError(t, err)
	require.Zero(t, result)
	database.AssertExpectations(t)
	database.AssertNotCalled(t, "ListNodes", mock.Anything, mock.Anything)
}

func expectProvisionWorkflow(t *testing.T, temporalClient *mockWorkflowStarter, nodeID uuid.UUID) {
	t.Helper()
	temporalClient.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(opts client.StartWorkflowOptions) bool {
		return opts.TaskQueue == "test-task-queue" &&
			opts.ID == constants.ProvisionNodeWorkflowPrefix+nodeID.String()
	}), mock.Anything, mock.MatchedBy(func(input workflows.ProvisionNodeInput) bool {
		return input.NodeID == nodeID && input.RebootNodeWaitForUptimeTimeout == 5*time.Minute
	})).Return(mockWorkflowRun{id: nodeID.String()}, nil).Once()
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
