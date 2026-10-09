// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

// Execute real cleanup and condition activities against a stateful store, then
// feed their persisted rows to serial-join admission. This is not a PostgreSQL
// row-lock or live kubeadm qualification.
func TestPreJoinCleanupDoesNotStallNextControlPlaneJoin(t *testing.T) {
	ctx := t.Context()
	cluster := db.Cluster{ID: uuid.New(), Conditions: db.ClusterConditions{Provisioned: db.NewCondition(true, "provisioned")}}
	target := readyNode("cp-joining", time.Now())
	target.ClusterID, target.ControlPlane = cluster.ID, true
	target.Conditions.Provisioning = db.NewCondition(true, "joining")
	peer := db.Node{
		ID: uuid.New(), ClusterID: cluster.ID, ControlPlane: true, Name: "cp-0", IP: "10.0.0.10",
		Conditions: db.NodeConditions{Joined: db.NewCondition(true, "joined"), Provisioned: db.NewCondition(true, "provisioned")},
	}
	store := &cleanupJoinStore{cluster: cluster, nodes: []db.Node{peer, target}}

	agent := &agenttest.FakeClient{}
	a := activities.NewActivities(config.Config{}, store, nil, agent, "", nil, "")
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(a.RemoveEtcdMember)
	env.RegisterActivity(a.KubeadmReset)
	_, err := env.ExecuteActivity(a.RemoveEtcdMember, activities.EtcdMemberRemovalInput{
		ClusterID: cluster.ID, TargetNodeID: target.ID, TargetMemberName: target.Name,
	})
	require.NoError(t, err)
	_, err = env.ExecuteActivity(a.KubeadmReset, activities.KubeadmResetInput{
		NodeID: target.ID, SkipPhases: []string{"remove-etcd-member"},
	})
	require.NoError(t, err)
	require.Len(t, agent.RemoveEtcdMemberCalls, 1)

	// Apply the normal join and post-join completion updates. Deleting must not
	// need a special completion update: the workflow never writes one.
	require.NoError(t, a.UpdateNodeConditions(ctx, activities.NodeConditionsUpdate{
		NodeID: target.ID, Conditions: activities.NodeConditions{
			Joined: &activities.ConditionUpdate{Status: db.ConditionStatusTrue},
		},
	}))
	require.NoError(t, a.UpdateNodeConditions(ctx, activities.NodeConditionsUpdate{
		NodeID: target.ID, Conditions: activities.NodeConditions{
			Provisioning: &activities.ConditionUpdate{Status: db.ConditionStatusFalse},
			Provisioned:  &activities.ConditionUpdate{Status: db.ConditionStatusTrue},
		},
	}))

	// Enqueue another CP join and use the real reconciler, not just a check of
	// Deleting, to prove that the preceding cleanup did not strand its slot.
	next := readyNode("cp-next", time.Now())
	next.ClusterID, next.ControlPlane = cluster.ID, true
	store.nodes = append(store.nodes, next)
	nodes, err := store.ListNodes(ctx, db.NodeFilters{ClusterID: &cluster.ID})
	require.NoError(t, err)
	temporal := &fencedTemporal{}
	r := &reconciler{
		cfg:    config.Config{ProvisionControlPlaneWithControlPlaneController: true},
		logger: slog.New(slog.DiscardHandler), controllerTemporalClient: temporal,
	}
	inFlight, err := r.reconcileControlPlaneJoins(ctx, cluster, &controlPlane{nodes: nodes})
	require.NoError(t, err)
	require.True(t, inFlight)
	require.True(t, temporal.started, "completed pre-join cleanup must not block the next CP join")
}

// cleanupJoinStore models committed node conditions across actual activities.
// Like the activity fenceStore, it serializes transactions but not PostgreSQL.
type cleanupJoinStore struct {
	db.DB
	mu      sync.Mutex
	cluster db.Cluster
	nodes   []db.Node
}

func (s *cleanupJoinStore) RunInTx(ctx context.Context, fn func(context.Context, db.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(ctx, s)
}

func (s *cleanupJoinStore) GetClusterForUpdate(context.Context, uuid.UUID) (db.Cluster, error) {
	return s.cluster, nil
}

func (s *cleanupJoinStore) GetNode(_ context.Context, id uuid.UUID) (db.Node, error) {
	for _, node := range s.nodes {
		if node.ID == id {
			return node, nil
		}
	}
	return db.Node{}, db.ErrNotFound
}

func (s *cleanupJoinStore) GetNodeForUpdate(ctx context.Context, id uuid.UUID) (db.Node, error) {
	return s.GetNode(ctx, id)
}

func (s *cleanupJoinStore) UpdateNode(_ context.Context, node *db.Node) (db.Node, error) {
	for i := range s.nodes {
		if s.nodes[i].ID == node.ID {
			s.nodes[i] = *node
			return *node, nil
		}
	}
	return db.Node{}, db.ErrNotFound
}

func (s *cleanupJoinStore) ListNodes(context.Context, db.NodeFilters) ([]db.Node, error) {
	return append([]db.Node(nil), s.nodes...), nil
}
