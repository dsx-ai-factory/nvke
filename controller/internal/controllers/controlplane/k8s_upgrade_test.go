// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/enums/v1"
	temporalworkflow "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	temporalmocks "go.temporal.io/sdk/mocks"
	"google.golang.org/protobuf/types/known/timestamppb"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func runningWorkflow() *workflowservice.DescribeWorkflowExecutionResponse {
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &temporalworkflow.WorkflowExecutionInfo{},
	}
}

func completedWorkflow() *workflowservice.DescribeWorkflowExecutionResponse {
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &temporalworkflow.WorkflowExecutionInfo{
			CloseTime: timestamppb.Now(),
			Status:    enums.WORKFLOW_EXECUTION_STATUS_COMPLETED,
		},
	}
}

// TestReconcileK8sUpgradeRequeuesWhileWaiting covers NKX-19294: a workflow
// finishing writes nothing to the DB, so every wait in the upgrade must ask for
// a short requeue instead of relying on the 15-minute resync.
func TestReconcileK8sUpgradeRequeuesWhileWaiting(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	node := healthyCPNode(clusterID, "cp-a", time.Now())
	drifted := healthyCPNode(clusterID, "cp-drifted", time.Now())
	drifted.ControlPlaneConfigurationID = uuid.New()
	upgrading := db.Cluster{
		ID:                clusterID,
		Name:              "test-cluster",
		KubernetesVersion: "v1.34.2",
		Conditions: db.ClusterConditions{
			ControlPlaneUpgrading: db.NewCondition(true, "upgrading"),
		},
	}
	readyCP := func() *controlPlane {
		return &controlPlane{
			nodes:                     []db.Node{node},
			clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 1},
		}
	}

	tests := []struct {
		name        string
		cluster     db.Cluster
		cp          *controlPlane
		setup       func(*temporalmocks.Client, *testutil.MockTx)
		wantRequeue time.Duration
	}{
		{
			name: "kubeadm ConfigMap workflow running",
			cluster: db.Cluster{
				ID:                clusterID,
				KubernetesVersion: "v1.34.2",
			},
			cp: &controlPlane{
				nodes: []db.Node{drifted},
				nodePinnedControlPlaneConfigs: map[uuid.UUID]db.ControlPlaneConfigSpec{
					drifted.ControlPlaneConfigurationID: {KubernetesVersion: "v1.33.6"},
				},
			},
			setup: func(tc *temporalmocks.Client, _ *testutil.MockTx) {
				tc.On("DescribeWorkflowExecution", mock.Anything, mock.Anything, "").Return(runningWorkflow(), nil).Once()
			},
			wantRequeue: upgradeWaitRequeueInterval,
		},
		{
			name: "control-plane nodes still recycling",
			cluster: func() db.Cluster {
				c := upgrading
				c.Conditions.CPNodesRecycling = db.NewCondition(true, "recycling")
				return c
			}(),
			cp:          readyCP(),
			setup:       func(*temporalmocks.Client, *testutil.MockTx) {},
			wantRequeue: upgradeWaitRequeueInterval,
		},
		{
			name:    "add-on upgrade workflow running",
			cluster: upgrading,
			cp:      readyCP(),
			setup: func(tc *temporalmocks.Client, _ *testutil.MockTx) {
				tc.On("DescribeWorkflowExecution", mock.Anything, mock.Anything, "").Return(runningWorkflow(), nil).Once()
			},
			wantRequeue: upgradeWaitRequeueInterval,
		},
		{
			name:    "add-on upgrade workflow done marks upgrade complete without requeue",
			cluster: upgrading,
			cp:      readyCP(),
			setup: func(tc *temporalmocks.Client, tx *testutil.MockTx) {
				tc.On("DescribeWorkflowExecution", mock.Anything, mock.Anything, "").Return(completedWorkflow(), nil).Once()
				tx.On("GetCluster", mock.Anything, clusterID).Return(upgrading, nil).Once()
				tx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
					return c.Conditions.ControlPlaneUpgrading.Status == db.ConditionStatusFalse
				})).Return(db.Cluster{}, nil).Once()
			},
		},
		{
			name:    "no upgrade needed",
			cluster: db.Cluster{ID: clusterID},
			cp:      &controlPlane{},
			setup:   func(*temporalmocks.Client, *testutil.MockTx) {},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			temporalClient := &temporalmocks.Client{}
			mockTx := &testutil.MockTx{}
			tt.setup(temporalClient, mockTx)
			r := &reconciler{
				db:                       mockTx,
				logger:                   slog.Default(),
				controllerTemporalClient: temporalClient,
			}

			require.NoError(t, r.reconcileK8sUpgrade(t.Context(), tt.cluster, tt.cp))
			assert.Equal(t, tt.wantRequeue, r.requeue)
			temporalClient.AssertExpectations(t)
			mockTx.AssertExpectations(t)
		})
	}
}

func TestMergeRequeue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		result  reconcile.Result
		requeue time.Duration
		want    time.Duration
	}{
		{name: "neither asks", want: 0},
		{name: "only control plane asks", requeue: 15 * time.Second, want: 15 * time.Second},
		{name: "only agent upgrade asks", result: reconcile.Result{RequeueAfter: 30 * time.Second}, want: 30 * time.Second},
		{name: "control plane sooner", result: reconcile.Result{RequeueAfter: 30 * time.Second}, requeue: 15 * time.Second, want: 15 * time.Second},
		{name: "agent upgrade sooner", result: reconcile.Result{RequeueAfter: 10 * time.Second}, requeue: 15 * time.Second, want: 10 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, mergeRequeue(tt.result, tt.requeue).RequeueAfter)
		})
	}
}
