// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

type fenceStore struct {
	db.DB
	mu        sync.Mutex
	cluster   db.Cluster
	nodes     []db.Node
	pools     []db.NodePool
	writes    int
	lockOrder []string
}

func (s *fenceStore) RunInTx(ctx context.Context, fn func(context.Context, db.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lockOrder = nil
	return fn(ctx, s)
}

func (s *fenceStore) GetClusterForUpdate(context.Context, uuid.UUID) (db.Cluster, error) {
	s.lockOrder = append(s.lockOrder, "cluster")
	return s.cluster, nil
}

func (s *fenceStore) GetCluster(context.Context, uuid.UUID) (db.Cluster, error) {
	return s.cluster, nil
}

func (s *fenceStore) UpdateCluster(_ context.Context, cluster *db.Cluster) (db.Cluster, error) {
	s.cluster = *cluster
	s.writes++
	return *cluster, nil
}

func (s *fenceStore) GetNode(_ context.Context, id uuid.UUID) (db.Node, error) {
	for _, node := range s.nodes {
		if node.ID == id {
			return node, nil
		}
	}
	return db.Node{}, db.ErrNotFound
}

func (s *fenceStore) GetNodeForUpdate(ctx context.Context, id uuid.UUID) (db.Node, error) {
	s.lockOrder = append(s.lockOrder, "node")
	return s.GetNode(ctx, id)
}

func (s *fenceStore) UpdateNode(_ context.Context, node *db.Node) (db.Node, error) {
	for i := range s.nodes {
		if s.nodes[i].ID == node.ID {
			s.nodes[i] = *node
			s.writes++
			return *node, nil
		}
	}
	return db.Node{}, db.ErrNotFound
}

func (s *fenceStore) ListNodes(context.Context, db.NodeFilters) ([]db.Node, error) {
	return append([]db.Node(nil), s.nodes...), nil
}

func (s *fenceStore) ListNodePools(context.Context, db.NodePoolFilters) ([]db.NodePool, error) {
	return s.pools, nil
}

func newFenceStore() *fenceStore {
	cluster := db.Cluster{ID: uuid.New(), Conditions: db.ClusterConditions{Provisioned: db.NewCondition(true, "ready"), UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeCertRenewal}}}
	return &fenceStore{cluster: cluster, nodes: []db.Node{stableRenewalNode(cluster.ID)}}
}

func fenceActivities(store *fenceStore, agent *agenttest.FakeClient) *activities.Activities {
	return activities.NewActivities(config.Config{}, store, nil, agent, "", nil, "")
}

// beginFenceOwner accepts ownership for either credential operation through its production activity.
func beginFenceOwner(t *testing.T, a *activities.Activities, store *fenceStore) error {
	t.Helper()
	_, err := a.BeginCertificateRenewal(t.Context(), activities.BeginCertificateRenewalInput{ClusterID: store.cluster.ID, CycleID: "cycle"})
	return err
}

func TestRenewalBeginRejectsPreviouslyAcceptedMembershipWork(t *testing.T) {
	for _, c := range ownedOperations("not_joined", "not_provisioned", "provisioning", "delete_requested", "deleting", "rebooting", "pool_deleting") {
		state := c.operation
		t.Run(string(c.owner)+"/"+state, func(t *testing.T) {
			store := newFenceStore()
			store.cluster.Conditions.UpdateInProgress = nil
			node := &store.nodes[0]
			switch state {
			case "not_joined":
				node.Conditions.Joined = db.NewCondition(false, "")
			case "not_provisioned":
				node.Conditions.Provisioned = db.NewCondition(false, "")
			case "provisioning":
				node.Conditions.Provisioning = db.NewCondition(true, "")
			case "delete_requested":
				node.Conditions.DeleteRequested = db.NewCondition(true, "")
			case "deleting":
				node.Conditions.Deleting = db.NewCondition(true, "")
			case "rebooting":
				node.Conditions.Rebooting = db.NewCondition(true, "")
			case "pool_deleting":
				store.pools = []db.NodePool{{Conditions: db.NodePoolConditions{Deleting: db.NewCondition(true, "")}}}
			}
			require.Error(t, beginFenceOwner(t, fenceActivities(store, nil), store))
			require.Nil(t, store.cluster.Conditions.UpdateInProgress)
			require.Zero(t, store.writes)
		})
	}
}

func TestControlPlaneAdmissionAndRenewalAreExclusive(t *testing.T) {
	for _, c := range ownedOperations("join", "teardown", "reboot") {
		mutation := c.operation
		for _, renewalFirst := range []bool{true, false} {
			t.Run(string(c.owner)+"/"+mutation+map[bool]string{true: "_renewal_first", false: "_mutation_first"}[renewalFirst], func(t *testing.T) {
				store := newFenceStore()
				store.cluster.Conditions.UpdateInProgress = nil
				a := fenceActivities(store, nil)
				update := activities.NodeConditionsUpdate{NodeID: store.nodes[0].ID}
				condition := &activities.ConditionUpdate{Status: db.ConditionStatusTrue, Message: "accepted work"}
				switch mutation {
				case "join":
					update.Conditions.Provisioning = condition
				case "teardown":
					update.Conditions.Deleting = condition
				case "reboot":
					update.Conditions.Rebooting = condition
				}
				begin := func() error { return beginFenceOwner(t, a, store) }
				if renewalFirst {
					require.NoError(t, begin())
					require.ErrorIs(t, a.UpdateNodeConditions(t.Context(), update), db.ErrControlPlaneMutationBlocked)
					require.Equal(t, []string{"cluster"}, store.lockOrder)
					require.NotEqual(t, db.ConditionStatusTrue, store.nodes[0].Conditions.Provisioning.Status)
				} else {
					require.NoError(t, a.UpdateNodeConditions(t.Context(), update))
					require.Equal(t, []string{"cluster", "node"}, store.lockOrder)
					require.Error(t, begin())
					require.Nil(t, store.cluster.Conditions.UpdateInProgress)
				}
			})
		}
	}
}

// fenceOwners are the operations whose retained ownership must fence control-plane mutations.
var fenceOwners = []db.OperationType{db.OperationTypeCertRenewal}

type ownedOperation struct {
	owner     db.OperationType
	operation string
}

func ownedOperations(operations ...string) []ownedOperation {
	cases := make([]ownedOperation, 0, len(operations)*len(fenceOwners))
	for _, owner := range fenceOwners {
		for _, operation := range operations {
			cases = append(cases, ownedOperation{owner: owner, operation: operation})
		}
	}
	return cases
}

func TestQueuedControlPlaneActivitiesStopBeforeSideEffects(t *testing.T) {
	for _, c := range ownedOperations("join", "reset", "soft_reboot", "hard_reboot", "remove_member", "delete_machine", "upgrade") {
		operation := c.operation
		t.Run(string(c.owner)+"/"+operation, func(t *testing.T) {
			store := newFenceStore()
			store.cluster.Conditions.UpdateInProgress.Type = c.owner
			a := fenceActivities(store, nil)
			var activity interface{}
			var input interface{}
			nodeID := store.nodes[0].ID
			switch operation {
			case "join":
				store.nodes[0].Conditions.Joined = db.NewCondition(false, "")
				activity = a.KubeadmJoin
				input = activities.KubeadmJoinInput{NodeID: nodeID}
			case "reset":
				activity = a.KubeadmReset
				input = activities.KubeadmResetInput{NodeID: nodeID}
			case "soft_reboot":
				activity = a.SoftRebootNode
				input = activities.SoftRebootNodeInput{NodeID: nodeID}
			case "hard_reboot":
				activity = a.ApplyCAPIMachineRebootAnnotation
				input = activities.ApplyCAPIMachineRebootAnnotationInput{NodeID: nodeID}
			case "remove_member":
				activity = a.RemoveEtcdMember
				input = activities.EtcdMemberRemovalInput{ClusterID: store.cluster.ID, TargetNodeID: nodeID, TargetMemberName: store.nodes[0].Name}
			case "delete_machine":
				activity = a.DeleteCAPIMachineForNode
				input = activities.DeleteCAPIMachineForNodeInput{NodeID: nodeID}
			case "upgrade":
				activity = a.KubeadmUpgradeApply
				input = activities.KubeadmUpgradeApplyInput{ClusterID: store.cluster.ID, KubernetesVersion: "1.34.2"}
			}
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivity(activity)
			_, err := env.ExecuteActivity(activity, input)
			require.ErrorContains(t, err, db.ErrControlPlaneMutationBlocked.Error())
			require.Zero(t, store.writes)
			// Nil agent and CAPI clients ensure any escaped side effect panics/fails.
		})
	}
}

func TestInFlightUpgradeRetainsAdmissionUntilConfirmedSuccess(t *testing.T) {
	for _, owner := range fenceOwners {
		for _, successful := range []bool{false, true} {
			t.Run(string(owner)+"/"+map[bool]string{false: "uncertain_failure", true: "confirmed_success"}[successful], func(t *testing.T) {
				store := newFenceStore()
				store.cluster.Conditions.UpdateInProgress = nil
				agent := &agenttest.FakeClient{KubeadmUpgradeApplyResponse: &nvkeproto.KubeadmUpgradeApplyResponse{Outcome: nvkeproto.KubeadmUpgradeApplyResponse_OUTCOME_APPLIED}}
				if !successful {
					agent.KubeadmUpgradeApplyError = errors.New("RPC outcome uncertain")
				}
				a := fenceActivities(store, agent)
				var suite testsuite.WorkflowTestSuite
				env := suite.NewTestActivityEnvironment()
				env.RegisterActivity(a.KubeadmUpgradeApply)
				_, err := env.ExecuteActivity(a.KubeadmUpgradeApply, activities.KubeadmUpgradeApplyInput{ClusterID: store.cluster.ID, KubernetesVersion: "1.34.2"})
				if successful {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
				require.Len(t, agent.KubeadmUpgradeApplyCalls, 1)
				require.Equal(t, []string{"cluster", "node"}, store.lockOrder)
				err = beginFenceOwner(t, a, store)
				if successful {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
					require.Nil(t, store.cluster.Conditions.UpdateInProgress)
					require.Equal(t, db.ConditionStatusTrue, store.nodes[0].Conditions.Provisioning.Status)
				}
			})
		}
	}
}

func TestRenewalPreservesWorkerAdmissionsAndControlPlaneObservations(t *testing.T) {
	store := newFenceStore()
	store.cluster.Conditions.UpdateInProgress = &db.UpdateInProgress{Type: db.OperationTypeCertRenewal}
	a := fenceActivities(store, nil)
	require.NoError(t, a.UpdateNodeConditions(t.Context(), activities.NodeConditionsUpdate{
		NodeID:     store.nodes[0].ID,
		Conditions: activities.NodeConditions{NodeAgentReady: &activities.ConditionUpdate{Status: db.ConditionStatusFalse, Message: "unreachable"}},
	}))
	require.Equal(t, []string{"node"}, store.lockOrder)
	store.nodes[0].ControlPlane = false
	require.NoError(t, a.UpdateNodeConditions(t.Context(), activities.NodeConditionsUpdate{
		NodeID:     store.nodes[0].ID,
		Conditions: activities.NodeConditions{Provisioning: &activities.ConditionUpdate{Status: db.ConditionStatusTrue, Message: "worker join"}},
	}))
	require.Equal(t, []string{"node"}, store.lockOrder)
}

// TestRetainedFailedRenewalKeepsFencingUntilReleased protects an unconfirmed
// renewal executor: a failed cycle that retains ownership must keep deferring
// control-plane work, and only released ownership lets it resume.
func TestRetainedFailedRenewalKeepsFencingUntilReleased(t *testing.T) {
	for _, retain := range []bool{true, false} {
		t.Run(map[bool]string{true: "retained", false: "released"}[retain], func(t *testing.T) {
			store := newFenceStore()
			store.cluster.Conditions.UpdateInProgress = nil
			a := fenceActivities(store, nil)
			require.NoError(t, beginFenceOwner(t, a, store))
			_, err := a.FailCertificateRenewal(t.Context(), activities.FailCertificateRenewalInput{
				ClusterID: store.cluster.ID, CycleID: "cycle", RetainMutationLock: retain, Phase: "renew", Message: "outcome unknown",
			})
			require.NoError(t, err)
			require.Equal(t, db.CertificateRenewalStateFailed, store.cluster.Conditions.CertificateRenewal.State)
			err = a.UpdateNodeConditions(t.Context(), activities.NodeConditionsUpdate{
				NodeID:     store.nodes[0].ID,
				Conditions: activities.NodeConditions{Rebooting: &activities.ConditionUpdate{Status: db.ConditionStatusTrue, Message: "reboot"}},
			})
			if retain {
				require.ErrorIs(t, err, db.ErrControlPlaneMutationBlocked)
				require.NotEqual(t, db.ConditionStatusTrue, store.nodes[0].Conditions.Rebooting.Status)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func stableRenewalNode(clusterID uuid.UUID) db.Node {
	return db.Node{
		ID: uuid.New(), ClusterID: clusterID, ControlPlane: true, Name: "cp-0", IP: "10.0.0.10",
		Conditions: db.NodeConditions{Joined: db.NewCondition(true, "joined"), Provisioned: db.NewCondition(true, "provisioned"), NodeAgentReady: db.NewCondition(true, "ready")},
	}
}

// TestClusterDeletionPreemptsCredentialOwners: teardown writers are admitted
// once deletion starts, even while a cycle holds or retains the mutation lock,
// and the renewal health gate stops the cycle before its next node.
func TestClusterDeletionPreemptsCredentialOwners(t *testing.T) {
	for _, owner := range fenceOwners {
		t.Run(string(owner), func(t *testing.T) {
			store := newFenceStore()
			store.cluster.Conditions.UpdateInProgress.Type = owner
			a := fenceActivities(store, nil)
			deleting := &activities.ConditionUpdate{Status: db.ConditionStatusTrue, Message: "Tearing down"}

			require.NoError(t, a.UpdateClusterConditions(t.Context(), activities.ClusterConditionsUpdate{
				ClusterID: store.cluster.ID, Conditions: activities.ClusterConditions{Deleting: deleting},
			}))
			require.Equal(t, db.ConditionStatusTrue, store.cluster.Conditions.Deleting.Status)
			require.NoError(t, a.UpdateNodeConditions(t.Context(), activities.NodeConditionsUpdate{
				NodeID: store.nodes[0].ID, Conditions: activities.NodeConditions{Deleting: deleting},
			}))
			require.Equal(t, db.ConditionStatusTrue, store.nodes[0].Conditions.Deleting.Status)

			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivity(a.ValidateCertificateRenewalHealth)
			_, err := env.ExecuteActivity(a.ValidateCertificateRenewalHealth, activities.ValidateCertificateRenewalHealthInput{
				ClusterID: store.cluster.ID, ExpectedNodeIDs: []uuid.UUID{store.nodes[0].ID},
			})
			var appErr *temporal.ApplicationError
			require.ErrorAs(t, err, &appErr)
			require.True(t, appErr.NonRetryable())
			require.ErrorContains(t, err, "cluster deletion is in progress")
		})
	}
}
