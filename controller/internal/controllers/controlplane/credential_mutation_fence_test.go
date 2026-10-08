// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/serviceerror"
	temporalapi "go.temporal.io/api/workflowservice/v1"
	tc "go.temporal.io/sdk/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

type fencedTemporal struct {
	tc.Client
	started bool
}

func (r *fencedTemporal) DescribeWorkflowExecution(context.Context, string, string) (*temporalapi.DescribeWorkflowExecutionResponse, error) {
	return nil, serviceerror.NewNotFound("not found")
}

func (r *fencedTemporal) ExecuteWorkflow(context.Context, tc.StartWorkflowOptions, interface{}, ...interface{}) (tc.WorkflowRun, error) {
	r.started = true
	return fencedRun{}, nil
}

type fencedRun struct{ tc.WorkflowRun }

func (fencedRun) GetID() string    { return "join-cp" }
func (fencedRun) GetRunID() string { return "run" }

// fenceOwners are the operations whose retained ownership must fence control-plane mutations.
var fenceOwners = []db.OperationType{db.OperationTypeEtcdKeyRotation, db.OperationTypeCertRenewal}

func TestControlPlaneJoinDefersDuringCredentialOperation(t *testing.T) {
	for _, owner := range fenceOwners {
		t.Run(string(owner), func(t *testing.T) {
			temporal := &fencedTemporal{}
			r := &reconciler{cfg: config.Config{}, logger: slog.New(slog.DiscardHandler), controllerTemporalClient: temporal}
			cluster := db.Cluster{ID: uuid.New(), Conditions: db.ClusterConditions{Provisioned: db.NewCondition(true, "ready"), UpdateInProgress: &db.UpdateInProgress{Type: owner}}}
			cp := &controlPlane{nodes: []db.Node{readyNode("replacement", time.Now())}}
			inFlight, err := r.reconcileControlPlaneJoins(t.Context(), cluster, cp)
			require.NoError(t, err)
			require.False(t, inFlight)
			require.False(t, temporal.started)
		})
	}
}

func TestControlPlaneReconcileDefersMembership(t *testing.T) {
	for _, owner := range fenceOwners {
		t.Run(string(owner), func(t *testing.T) {
			cluster := db.Cluster{ID: uuid.New(), ProviderType: db.ProviderTypeCAPI, ControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 3}, Conditions: db.ClusterConditions{Provisioned: db.NewCondition(true, "ready"), UpdateInProgress: &db.UpdateInProgress{Type: owner}}}
			tx := &testutil.MockTx{}
			store := &testutil.MockDB{MockTx: tx}
			store.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
			tx.On("GetClusterForUpdate", mock.Anything, cluster.ID).Return(cluster, nil).Once()
			tx.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{}, nil).Once()
			tx.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{}, nil).Maybe()
			temporal := &fencedTemporal{}
			controller := NewController(config.Config{}, slog.Default(), dbwrapper.New(store, slog.Default()), temporal, nil, nil, nil)
			_, err := controller.Reconcile(t.Context(), dbreconcile.ClusterKey(cluster.ID))
			require.NoError(t, err)
			require.False(t, temporal.started)
			tx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
			tx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			tx.AssertExpectations(t)
		})
	}
}
