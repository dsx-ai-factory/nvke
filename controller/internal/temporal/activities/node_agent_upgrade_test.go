// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

type agentAdmissionDatabase struct {
	db.DB
	mu      sync.Mutex
	cluster db.Cluster
	node    db.Node
}

func (d *agentAdmissionDatabase) RunInTx(ctx context.Context, fn func(context.Context, db.Tx) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return fn(ctx, d)
}

func (d *agentAdmissionDatabase) GetClusterForUpdate(context.Context, uuid.UUID) (db.Cluster, error) {
	return d.cluster, nil
}

func (d *agentAdmissionDatabase) GetNode(context.Context, uuid.UUID) (db.Node, error) {
	return d.node, nil
}

func (d *agentAdmissionDatabase) GetNodeForUpdate(ctx context.Context, id uuid.UUID) (db.Node, error) {
	return d.GetNode(ctx, id)
}

func (d *agentAdmissionDatabase) UpdateCluster(_ context.Context, cluster *db.Cluster) (db.Cluster, error) {
	d.cluster = *cluster
	return *cluster, nil
}

func (d *agentAdmissionDatabase) UpdateNode(_ context.Context, node *db.Node) (db.Node, error) {
	d.node = *node
	return *node, nil
}

type admissionAgentClient struct {
	*agenttest.FakeClient
	converge func(context.Context, db.Node, string, string) (string, bool, error)
}

func (c admissionAgentClient) ConvergeNodeAgent(ctx context.Context, node db.Node, target, method string) (string, bool, error) {
	return c.converge(ctx, node, target, method)
}

func TestClusterDeletionStopsLifecycleAgentUpgradeRetries(t *testing.T) {
	clusterID, nodeID := uuid.New(), uuid.New()
	database := &agentAdmissionDatabase{
		cluster: db.Cluster{ID: clusterID, Conditions: db.ClusterConditions{Deleting: db.NewCondition(true, "Cluster deletion is pending")}},
		node:    db.Node{ID: nodeID, ClusterID: clusterID, ControlPlane: true},
	}
	operations := &Activities{db: database, nodeAgentClient: admissionAgentClient{converge: func(context.Context, db.Node, string, string) (string, bool, error) {
		t.Fatal("cluster deletion must not start or resume an agent installation")
		return "", false, nil
	}}}
	err := operations.EnsureNodeAgentVersion(t.Context(), EnsureNodeAgentVersionInput{NodeID: nodeID, Version: "v0.1.2", Method: "apt"})
	require.True(t, temporal.IsCanceledError(err))
	require.Nil(t, database.cluster.NodeAgentUpgrade)
}

func TestEnsureNodeAgentVersionSavesProgressBeforeRetry(t *testing.T) {
	for _, test := range []struct {
		name      string
		done      bool
		callErr   error
		wantPhase string
		wantError string
	}{
		{name: "desired version is ready", done: true},
		{name: "accepted retry is running", wantPhase: "Running", wantError: "in progress"},
		{name: "failed call remains selected", callErr: status.Error(codes.Unavailable, "agent restarting"), wantPhase: "Running", wantError: "agent restarting"},
	} {
		t.Run(test.name, func(t *testing.T) {
			clusterID, nodeID := uuid.New(), uuid.New()
			active := &db.NodeAgentUpgrade{Source: db.NodeAgentUpgradeSourceLifecycle, NodeID: nodeID, TargetVersion: "v0.1.2", Method: "apt", Phase: "Running", ErrorMessage: "previous failure"}
			database := &agentAdmissionDatabase{cluster: db.Cluster{ID: clusterID, NodeAgentUpgrade: active}, node: db.Node{ID: nodeID, ClusterID: clusterID, ControlPlane: true}}
			operations := &Activities{db: database, nodeAgentClient: admissionAgentClient{converge: func(context.Context, db.Node, string, string) (string, bool, error) {
				return "v0.1.2", test.done, test.callErr
			}}}
			err := operations.EnsureNodeAgentVersion(t.Context(), EnsureNodeAgentVersionInput{NodeID: nodeID, Version: "v0.1.2", Method: "apt"})
			if test.wantError == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, test.wantError)
			}
			require.Equal(t, "v0.1.2", database.node.AgentVersion)
			if test.wantPhase == "" {
				require.Nil(t, database.cluster.NodeAgentUpgrade)
				return
			}
			require.NotNil(t, database.cluster.NodeAgentUpgrade)
			require.Equal(t, nodeID, database.cluster.NodeAgentUpgrade.NodeID)
			require.Equal(t, test.wantPhase, database.cluster.NodeAgentUpgrade.Phase)
			if test.callErr != nil {
				require.Equal(t, test.callErr.Error(), database.cluster.NodeAgentUpgrade.ErrorMessage)
			} else {
				require.Equal(t, "previous failure", database.cluster.NodeAgentUpgrade.ErrorMessage)
			}
		})
	}
}

func TestLifecycleAgentRPCDoesNotHoldDeletionTransaction(t *testing.T) {
	clusterID, nodeID := uuid.New(), uuid.New()
	database := &agentAdmissionDatabase{cluster: db.Cluster{ID: clusterID}, node: db.Node{ID: nodeID, ClusterID: clusterID, ControlPlane: true}}
	operations := &Activities{db: database, nodeAgentClient: admissionAgentClient{converge: func(context.Context, db.Node, string, string) (string, bool, error) {
		require.True(t, database.mu.TryLock(), "agent RPC must not hold the database transaction")
		database.node.Conditions.Deleting = db.NewCondition(true, "operator requested deletion")
		database.mu.Unlock()
		return "v0.1.2", false, nil
	}}}
	err := operations.EnsureNodeAgentVersion(t.Context(), EnsureNodeAgentVersionInput{NodeID: nodeID, Version: "v0.1.2", Method: "apt"})
	require.ErrorContains(t, err, "in progress")
	require.Equal(t, db.ConditionStatusTrue, database.node.Conditions.Deleting.Status)
}
