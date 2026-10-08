// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certrenewal

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/workflows"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

const certRenewalInterval = 12 * time.Hour

// TestUnit_ControllerReconcile keeps scheduling separate from completion: only
// the first look at a cluster sets its due time, start success or failure cannot
// advance it, and accepted or failed cycles must not be blindly relaunched by
// later reconciles.
func TestUnit_ControllerReconcile(t *testing.T) {
	for _, provisioned := range []db.ConditionStatus{db.ConditionStatusTrue, ""} {
		t.Run("schedules the first check without a workflow/provisioned="+string(provisioned), func(t *testing.T) {
			ctx := context.Background()
			clusterID := uuid.New()
			mockDB := &testutil.MockDB{}
			mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
				ID:         clusterID,
				Conditions: db.ClusterConditions{Provisioned: db.Condition{Status: provisioned}},
			}, nil).Once()
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
	}

	t.Run("starts due workflow without advancing due time", func(t *testing.T) {
		ctx := context.Background()
		clusterID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
		mockDB := &testutil.MockDB{}
		cluster := provisionedCertificateRenewalCluster(clusterID)
		due := *cluster.NextCertRenewalDue
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Once()
		controlPlane := true
		mockDB.On("ListNodes", mock.Anything, db.NodeFilters{ClusterID: &clusterID, ControlPlane: &controlPlane}).
			Return(readyCertificateRenewalNodes(clusterID), nil).Once()

		temporalClient := &mockWorkflowStarter{}
		temporalClient.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(opts client.StartWorkflowOptions) bool {
			return opts.TaskQueue == "test-task-queue" &&
				opts.ID == scheduledWorkflowID(clusterID, due) &&
				opts.WorkflowIDConflictPolicy == enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING &&
				opts.WorkflowIDReusePolicy == enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY
		}), mock.MatchedBy(func(candidate interface{}) bool {
			return reflect.ValueOf(candidate).Pointer() == reflect.ValueOf(workflows.RenewK8sCertsV2).Pointer()
		}), mock.MatchedBy(func(input workflows.RenewK8sCertsInput) bool {
			return input.ClusterID == clusterID && input.ThresholdDays == 14 && !input.Force && input.ScheduledFor.Equal(due)
		})).Return(mockWorkflowRun{id: "run"}, nil).Once()

		controller := newController(mockDB, temporalClient)
		result, err := controller.Reconcile(ctx, dbreconcile.ClusterKey(clusterID))

		require.NoError(t, err)
		require.Equal(t, reconcile.Result{RequeueAfter: time.Minute}, result)
		mockDB.AssertExpectations(t)
		temporalClient.AssertExpectations(t)
	})

	t.Run("uses due time in scheduled cycle", func(t *testing.T) {
		ctx := context.Background()
		clusterID := uuid.New()
		due := time.Now().Add(-time.Minute).UTC()
		mockDB := &testutil.MockDB{}
		cluster := provisionedCertificateRenewalCluster(clusterID)
		cluster.NextCertRenewalDue = &due
		cluster.Conditions.CertificateRenewal = &db.CertificateRenewalStatus{State: db.CertificateRenewalStateSucceeded}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Once()
		temporalClient := &mockWorkflowStarter{}
		temporalClient.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(opts client.StartWorkflowOptions) bool {
			return opts.ID == scheduledWorkflowID(clusterID, due)
		}), mock.Anything, mock.MatchedBy(func(input workflows.RenewK8sCertsInput) bool {
			return input.ScheduledFor != nil && input.ScheduledFor.Equal(due)
		})).Return(mockWorkflowRun{id: "run"}, nil).Once()

		result, err := newController(mockDB, temporalClient).Reconcile(ctx, dbreconcile.ClusterKey(clusterID))

		require.NoError(t, err)
		require.Equal(t, reconcile.Result{RequeueAfter: time.Minute}, result)
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
			Conditions: db.ClusterConditions{
				Provisioned: db.Condition{Status: db.ConditionStatusTrue},
			},
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

	t.Run("does not alter due time when workflow start fails", func(t *testing.T) {
		ctx := context.Background()
		clusterID := uuid.New()
		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(provisionedCertificateRenewalCluster(clusterID), nil).Once()
		controlPlane := true
		mockDB.On("ListNodes", mock.Anything, db.NodeFilters{ClusterID: &clusterID, ControlPlane: &controlPlane}).
			Return(readyCertificateRenewalNodes(clusterID), nil).Once()

		temporalClient := &mockWorkflowStarter{}
		temporalClient.On("ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(nil, errors.New("temporal unavailable")).Once()

		result, err := newController(mockDB, temporalClient).Reconcile(ctx, dbreconcile.ClusterKey(clusterID))

		require.ErrorContains(t, err, "start cert renewal workflow")
		require.Equal(t, reconcile.Result{}, result)
		mockDB.AssertExpectations(t)
		temporalClient.AssertExpectations(t)
	})

	t.Run("does not relaunch accepted scheduled cycle", func(t *testing.T) {
		ctx := context.Background()
		clusterID := uuid.New()
		due := time.Now().Add(-time.Minute).UTC()
		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
			ID:                 clusterID,
			NextCertRenewalDue: &due,
			Conditions: db.ClusterConditions{
				Provisioned: db.Condition{Status: db.ConditionStatusTrue},
				CertificateRenewal: &db.CertificateRenewalStatus{
					State: db.CertificateRenewalStateAccepted, ScheduledFor: &due,
				},
			},
		}, nil).Once()

		temporalClient := &mockWorkflowStarter{}
		result, err := newController(mockDB, temporalClient).Reconcile(ctx, dbreconcile.ClusterKey(clusterID))

		require.NoError(t, err)
		require.Equal(t, reconcile.Result{RequeueAfter: time.Minute}, result)
		temporalClient.AssertNotCalled(t, "ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("leaves failed scheduled cycle overdue without automatic restart", func(t *testing.T) {
		ctx := context.Background()
		clusterID := uuid.New()
		due := time.Now().Add(-time.Minute).UTC()
		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
			ID:                 clusterID,
			NextCertRenewalDue: &due,
			Conditions: db.ClusterConditions{
				Provisioned: db.Condition{Status: db.ConditionStatusTrue},
				CertificateRenewal: &db.CertificateRenewalStatus{
					State: db.CertificateRenewalStateFailed, ScheduledFor: &due, Phase: "postflight", Message: "etcd unhealthy",
				},
			},
		}, nil).Once()

		temporalClient := &mockWorkflowStarter{}
		result, err := newController(mockDB, temporalClient).Reconcile(ctx, dbreconcile.ClusterKey(clusterID))

		require.NoError(t, err)
		require.Equal(t, reconcile.Result{RequeueAfter: 15 * time.Minute}, result)
		temporalClient.AssertNotCalled(t, "ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("defers while another cluster mutation is in progress", func(t *testing.T) {
		ctx := context.Background()
		clusterID := uuid.New()
		mockDB := &testutil.MockDB{}
		due := time.Now().Add(-time.Minute)
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
			ID:                 clusterID,
			NextCertRenewalDue: &due,
			Conditions: db.ClusterConditions{
				Provisioned: db.Condition{Status: db.ConditionStatusTrue},
				UpdateInProgress: &db.UpdateInProgress{
					Type: db.OperationTypeK8sUpgrade,
				},
			},
		}, nil).Once()

		temporalClient := &mockWorkflowStarter{}
		result, err := newController(mockDB, temporalClient).Reconcile(ctx, dbreconcile.ClusterKey(clusterID))

		require.NoError(t, err)
		require.Equal(t, reconcile.Result{RequeueAfter: time.Minute}, result)
		temporalClient.AssertNotCalled(t, "ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("skips unprovisioned cluster", func(t *testing.T) {
		ctx := context.Background()
		clusterID := uuid.New()
		due := time.Now().Add(-time.Minute)
		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, NextCertRenewalDue: &due}, nil).Once()

		temporalClient := &mockWorkflowStarter{}
		result, err := newController(mockDB, temporalClient).Reconcile(ctx, dbreconcile.ClusterKey(clusterID))

		require.NoError(t, err)
		require.Equal(t, reconcile.Result{}, result)
		mockDB.AssertExpectations(t)
		temporalClient.AssertNotCalled(t, "ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
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
		result, err := newController(mockDB, temporalClient).Reconcile(ctx, dbreconcile.ClusterKey(clusterID))

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
		result, err := newController(mockDB, temporalClient).Reconcile(ctx, dbreconcile.ClusterKey(clusterID))

		require.NoError(t, err)
		require.Equal(t, reconcile.Result{}, result)
		mockDB.AssertExpectations(t)
		temporalClient.AssertNotCalled(t, "ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})
}

// TestUnit_ControllerReconcileWaitsForInitialControlPlane models Provisioned being
// set after only the first node joins. Starting then would consume the initial
// cycle with a health failure before the remaining control plane can become ready.
func TestUnit_ControllerReconcileWaitsForInitialControlPlane(t *testing.T) {
	ctx := context.Background()
	clusterID := uuid.New()
	cluster := provisionedCertificateRenewalCluster(clusterID)
	due := *cluster.NextCertRenewalDue
	mockDB := &testutil.MockDB{}
	temporalClient := &mockWorkflowStarter{}
	controller := newController(mockDB, temporalClient)
	controlPlane := true
	readyNodes := readyCertificateRenewalNodes(clusterID)

	steps := []struct {
		name            string
		nodeCount       int
		joined          db.ConditionStatus
		kubernetesReady db.ConditionStatus
		nodeAgentReady  db.ConditionStatus
	}{
		{"no control-plane nodes yet", 0, db.ConditionStatusTrue, db.ConditionStatusTrue, db.ConditionStatusTrue},
		{"only the initial node has joined", 1, db.ConditionStatusTrue, db.ConditionStatusTrue, db.ConditionStatusTrue},
		{"remaining nodes have not joined", 3, db.ConditionStatusFalse, db.ConditionStatusTrue, db.ConditionStatusTrue},
		{"remaining nodes are not Kubernetes ready", 3, db.ConditionStatusTrue, db.ConditionStatusUnknown, db.ConditionStatusTrue},
		{"remaining nodes are not agent ready", 3, db.ConditionStatusTrue, db.ConditionStatusTrue, ""},
		{"all control-plane nodes are ready", 3, db.ConditionStatusTrue, db.ConditionStatusTrue, db.ConditionStatusTrue},
	}
	for i, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			nodes := append([]db.Node(nil), readyNodes[:step.nodeCount]...)
			for j := 1; j < len(nodes); j++ {
				nodes[j].Conditions.Joined.Status = step.joined
				nodes[j].Conditions.KubernetesReady.Status = step.kubernetesReady
				nodes[j].Conditions.NodeAgentReady.Status = step.nodeAgentReady
			}
			mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Once()
			mockDB.On("ListNodes", mock.Anything, db.NodeFilters{ClusterID: &clusterID, ControlPlane: &controlPlane}).
				Return(nodes, nil).Once()
			ready := i == len(steps)-1
			if ready {
				temporalClient.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(opts client.StartWorkflowOptions) bool {
					return opts.ID == scheduledWorkflowID(clusterID, due)
				}), mock.Anything, mock.MatchedBy(func(input workflows.RenewK8sCertsInput) bool {
					return input.ClusterID == clusterID && input.ScheduledFor.Equal(due) && !input.Force
				})).Return(mockWorkflowRun{id: "run"}, nil).Once()
			}

			result, err := controller.Reconcile(ctx, dbreconcile.ClusterKey(clusterID))

			require.NoError(t, err)
			require.Equal(t, reconcile.Result{RequeueAfter: time.Minute}, result)
			mockDB.AssertExpectations(t)
			if ready {
				temporalClient.AssertExpectations(t)
			} else {
				temporalClient.AssertNotCalled(t, "ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			}
		})
	}
}

// TestUnit_ControllerReconcileStartsInitialRenewalForSingleControlPlane shows a
// one-node cluster is not deferred once its only node is ready.
func TestUnit_ControllerReconcileStartsInitialRenewalForSingleControlPlane(t *testing.T) {
	ctx := context.Background()
	clusterID := uuid.New()
	cluster := provisionedCertificateRenewalCluster(clusterID)
	cluster.ControlPlaneConfig.DesiredCount = 1
	due := *cluster.NextCertRenewalDue
	mockDB := &testutil.MockDB{}
	temporalClient := &mockWorkflowStarter{}
	controller := newController(mockDB, temporalClient)
	controlPlane := true
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Once()
	mockDB.On("ListNodes", mock.Anything, db.NodeFilters{ClusterID: &clusterID, ControlPlane: &controlPlane}).
		Return(readyCertificateRenewalNodes(clusterID)[:1], nil).Once()
	temporalClient.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(opts client.StartWorkflowOptions) bool {
		return opts.ID == scheduledWorkflowID(clusterID, due)
	}), mock.Anything, mock.MatchedBy(func(input workflows.RenewK8sCertsInput) bool {
		return input.ClusterID == clusterID && input.ScheduledFor.Equal(due) && !input.Force
	})).Return(mockWorkflowRun{id: "run"}, nil).Once()

	result, err := controller.Reconcile(ctx, dbreconcile.ClusterKey(clusterID))

	require.NoError(t, err)
	require.Equal(t, reconcile.Result{RequeueAfter: time.Minute}, result)
	mockDB.AssertExpectations(t)
	temporalClient.AssertExpectations(t)
}

func TestUnit_ControllerReconcileInitialControlPlaneListError(t *testing.T) {
	clusterID := uuid.New()
	mockDB := &testutil.MockDB{}
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(provisionedCertificateRenewalCluster(clusterID), nil).Once()
	controlPlane := true
	listErr := errors.New("database unavailable")
	mockDB.On("ListNodes", mock.Anything, db.NodeFilters{ClusterID: &clusterID, ControlPlane: &controlPlane}).Return(nil, listErr).Once()
	temporalClient := &mockWorkflowStarter{}

	result, err := newController(mockDB, temporalClient).Reconcile(context.Background(), dbreconcile.ClusterKey(clusterID))

	require.ErrorIs(t, err, listErr)
	require.Equal(t, reconcile.Result{}, result)
	mockDB.AssertExpectations(t)
	temporalClient.AssertNotCalled(t, "ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestScheduledCycleMatches(t *testing.T) {
	due := time.Now().UTC()
	same := due
	other := due.Add(time.Second)

	require.True(t, scheduledCycleMatches(&db.CertificateRenewalStatus{}, nil))
	require.True(t, scheduledCycleMatches(&db.CertificateRenewalStatus{ScheduledFor: &same}, &due))
	require.False(t, scheduledCycleMatches(&db.CertificateRenewalStatus{Forced: true, ScheduledFor: &same}, &due))
	require.False(t, scheduledCycleMatches(&db.CertificateRenewalStatus{ScheduledFor: &other}, &due))
	require.False(t, scheduledCycleMatches(nil, &due))
}

// provisionedCertificateRenewalCluster is due and has never accepted a cycle.
func provisionedCertificateRenewalCluster(clusterID uuid.UUID) db.Cluster {
	due := time.Now().Add(-time.Minute).UTC()
	return db.Cluster{
		ID:                 clusterID,
		NextCertRenewalDue: &due,
		ControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 3},
		Conditions: db.ClusterConditions{
			Provisioned: db.Condition{Status: db.ConditionStatusTrue},
		},
	}
}

func readyCertificateRenewalNodes(clusterID uuid.UUID) []db.Node {
	nodes := make([]db.Node, 3)
	for i := range nodes {
		nodes[i] = db.Node{
			ID:           uuid.New(),
			ClusterID:    clusterID,
			ControlPlane: true,
			Conditions: db.NodeConditions{
				Joined:          db.Condition{Status: db.ConditionStatusTrue},
				KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
				NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue},
			},
		}
	}
	return nodes
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
