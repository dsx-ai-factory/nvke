// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
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
		cfg:    config.Config{},
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
	outbox  []db.WorkflowOutbox
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

func (s *cleanupJoinStore) ListNodePools(context.Context, db.NodePoolFilters) ([]db.NodePool, error) {
	return nil, nil
}

func (s *cleanupJoinStore) CreateWorkflowOutboxEntry(_ context.Context, workflowType, workflowID string, input interface{}) (db.WorkflowOutbox, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return db.WorkflowOutbox{}, err
	}
	entry := db.WorkflowOutbox{WorkflowType: workflowType, WorkflowID: workflowID, WorkflowInput: payload}
	s.outbox = append(s.outbox, entry)
	return entry, nil
}

// Exercise request admission, real cleanup, normal condition completion and the
// whole controller transaction. In particular, Deleting must be paired with a
// teardown outbox entry by the reconciler, never invented by pre-join cleanup.
func TestPreJoinCleanupQueuedDeleteReachesTeardownOutbox(t *testing.T) {
	for _, siblingState := range []string{"stable", "queued", "deleting"} {
		t.Run(siblingState+"_sibling", func(t *testing.T) {
			ctx := t.Context()
			cluster := db.Cluster{
				ID: uuid.New(), Name: "cleanup-delete", ControlPlaneVIP: "10.0.0.100", ProviderType: db.ProviderTypeCAPI,
				ControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 2},
				Conditions:         db.ClusterConditions{Provisioned: db.NewCondition(true, "provisioned")},
			}
			target := readyNode("cp-joining", time.Now())
			target.ClusterID, target.ControlPlane = cluster.ID, true
			target.Conditions.Provisioning = db.NewCondition(true, "joining")
			peer := db.Node{
				ID: uuid.New(), ClusterID: cluster.ID, ControlPlane: true, Name: "cp-0", IP: "10.0.0.10",
				Conditions: db.NodeConditions{Joined: db.NewCondition(true, "joined"), Provisioned: db.NewCondition(true, "provisioned")},
			}
			sibling := peer
			sibling.ID, sibling.Name, sibling.IP = uuid.New(), "cp-sibling", "10.0.0.11"
			store := &cleanupJoinStore{cluster: cluster, nodes: []db.Node{peer, target, sibling}}
			requestDelete := func(id uuid.UUID) {
				require.NoError(t, nodes.Delete(ctx, store, id, nodes.WithAllowCPNode(true),
					nodes.WithReason("API")))
			}
			requestDelete(target.ID)
			requested := store.nodes[1]
			require.True(t, nodes.IsDeleteRequested(requested))
			require.False(t, nodes.IsDeleting(requested))
			require.Empty(t, store.outbox, "request admission must only record intent")
			if siblingState != "stable" {
				requestDelete(sibling.ID)
			}
			if siblingState == "deleting" {
				require.NoError(t, store.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
					return nodes.StartRequestedTeardown(ctx, tx, sibling.ID)
				}))
				require.Len(t, store.outbox, 1)
			}
			siblingBefore := store.nodes[2]
			outboxBefore := len(store.outbox)

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
			assert.False(t, nodes.IsDeleting(store.nodes[1]), "queued intent is not started teardown")
			require.Equal(t, db.ConditionStatusTrue, store.nodes[1].Conditions.Provisioning.Status)
			require.Equal(t, requested.Conditions.DeleteRequested, store.nodes[1].Conditions.DeleteRequested)
			require.Equal(t, requested.DeleteRequest, store.nodes[1].DeleteRequest)
			require.Equal(t, siblingBefore, store.nodes[2], "cleanup must not reclassify sibling intent or teardown")
			require.Len(t, store.outbox, outboxBefore, "cleanup must not enqueue teardown")

			// These are the existing workflow's join and completion updates; none
			// clears Deleting, because a genuine teardown must remain reserved.
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

			scheme := runtime.NewScheme()
			require.NoError(t, clusterv1.AddToScheme(scheme))
			capiCluster := &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: config.Config{}.ManagementNamespaceOrDefault()},
				Status:     clusterv1.ClusterStatus{Initialization: clusterv1.ClusterInitializationStatus{InfrastructureProvisioned: ptr.To(true)}},
			}
			k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(capiCluster).Build()
			temporal := &fencedTemporal{}
			logger := slog.New(slog.DiscardHandler)
			controller := NewController(config.Config{}, logger, dbwrapper.New(store, logger), temporal, k8sClient, k8sClient, nil)
			reconcile := func() {
				_, err := controller.Reconcile(ctx, dbreconcile.ClusterKey(cluster.ID))
				require.NoError(t, err)
			}
			if siblingState == "deleting" {
				reconcile()
				require.Len(t, store.outbox, outboxBefore, "real sibling teardown holds the serialized slot")
				require.True(t, nodes.IsDeleteRequested(store.nodes[1]))
				// Model the existing teardown's eventual row removal, not a
				// successful cleanup RPC clearing its deletion reservation.
				store.nodes = store.nodes[:2]
			}
			reconcile()
			require.Len(t, store.outbox, outboxBefore+1, "completed join must let the real reconciler queue requested teardown")
			require.True(t, nodes.IsDeleting(store.nodes[1]))
			require.False(t, nodes.IsDeleteRequested(store.nodes[1]))
			require.Equal(t, db.ConditionStatusFalse, store.nodes[1].Conditions.Provisioning.Status)
			entry := store.outbox[outboxBefore]
			require.Equal(t, constants.TeardownNodeWorkflowType, entry.WorkflowType)
			require.Equal(t, constants.TeardownNodeWorkflowPrefix+target.ID.String(), entry.WorkflowID)
			require.JSONEq(t, fmt.Sprintf(`{"NodeID":%q,"AllowCPNode":true,"Reason":"API","ForceUnreachable":true,"SkipRemoveFromCluster":false,"MachineHealthIssue":null}`, target.ID.String()), string(entry.WorkflowInput))
			if siblingState == "queued" {
				require.Equal(t, siblingBefore, store.nodes[2], "only the oldest request may be promoted")
			}

			next := readyNode("cp-next", time.Now())
			next.ClusterID, next.ControlPlane = cluster.ID, true
			store.nodes = append(store.nodes, next)
			reconcile()
			require.False(t, temporal.started, "genuine teardown now blocks later joins")
			require.Len(t, store.outbox, outboxBefore+1, "awaiting outbox delivery must not enqueue twice")
			// Remove the completed teardown row. A queued sibling, if present,
			// must not hold the next join: the controller grows before shrinking.
			store.nodes = append(store.nodes[:1], store.nodes[2:]...)
			reconcile()
			require.True(t, temporal.started, "only genuine teardown may block the next join")
			require.Len(t, store.outbox, outboxBefore+1)
		})
	}
}
