// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities_test

import (
	"context"
	"errors"
	"fmt"
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
	cycles    int
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
	cluster := db.Cluster{ID: uuid.New(), EtcdEncrypted: true, Conditions: db.ClusterConditions{Provisioned: db.NewCondition(true, "ready"), UpdateInProgress: &db.UpdateInProgress{Type: db.OperationTypeCertRenewal}}, KubernetesVersion: "1.34.0"}
	return &fenceStore{cluster: cluster, nodes: []db.Node{stableRenewalNode(cluster.ID)}}
}

func fenceActivities(store *fenceStore, agent *agenttest.FakeClient) *activities.Activities {
	return activities.NewActivities(config.Config{}, store, nil, agent, "", nil, "")
}

// beginFenceOwner accepts ownership for either credential operation through its production activity.
// Each call is a new cycle, as the controller's retries are: a refused renewal cycle is recorded as failed.
func beginFenceOwner(t *testing.T, a *activities.Activities, store *fenceStore, owner db.OperationType) error {
	t.Helper()
	if owner == db.OperationTypeEtcdKeyRotation {
		_, err := a.BeginEtcdEncryptionKeyRotation(t.Context(), activities.BeginEtcdEncryptionKeyRotationInput{ClusterID: store.cluster.ID, WorkflowID: "owner"})
		return err
	}
	store.cycles++
	_, err := a.BeginCertificateRenewal(t.Context(), activities.BeginCertificateRenewalInput{ClusterID: store.cluster.ID, CycleID: fmt.Sprintf("cycle-%d", store.cycles)})
	return err
}

func TestCredentialBeginRejectsPreviouslyAcceptedMembershipWork(t *testing.T) {
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
			require.Error(t, beginFenceOwner(t, fenceActivities(store, nil), store, c.owner))
			require.Nil(t, store.cluster.Conditions.UpdateInProgress)
			if c.owner == db.OperationTypeCertRenewal {
				// The refusal is recorded as a failed cycle, never as ownership.
				require.Equal(t, 1, store.writes)
				require.Equal(t, db.CertificateRenewalStateFailed, store.cluster.Conditions.CertificateRenewal.State)
			} else {
				require.Zero(t, store.writes)
			}
		})
	}
}

func TestControlPlaneAdmissionAndCredentialOperationAreExclusive(t *testing.T) {
	for _, c := range ownedOperations("join", "teardown", "reboot") {
		mutation := c.operation
		for _, credentialFirst := range []bool{true, false} {
			t.Run(string(c.owner)+"/"+mutation+map[bool]string{true: "_credential_first", false: "_mutation_first"}[credentialFirst], func(t *testing.T) {
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
				begin := func() error { return beginFenceOwner(t, a, store, c.owner) }
				if credentialFirst {
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
var fenceOwners = []db.OperationType{db.OperationTypeEtcdKeyRotation, db.OperationTypeCertRenewal}

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
				err = beginFenceOwner(t, a, store, owner)
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
			require.NoError(t, beginFenceOwner(t, a, store, db.OperationTypeCertRenewal))
			_, err := a.FailCertificateRenewal(t.Context(), activities.FailCertificateRenewalInput{
				ClusterID: store.cluster.ID, CycleID: store.cluster.Conditions.CertificateRenewal.CycleID, RetainMutationLock: retain, Phase: "renew", Message: "outcome unknown",
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

// TestFailedV1StartReleasesReservationForV2Begin: a V1 history never resolves its
// operation and the reconciler keeps unfenced records, so a failed V1 Start must
// release its reservation instead of blocking later cycles and agent upgrades.
func TestFailedV1StartReleasesReservationForV2Begin(t *testing.T) {
	store := newFenceStore()
	store.cluster.Conditions.UpdateInProgress = nil
	agent := &agenttest.FakeClient{
		StartKubernetesCertificateRenewalHook: func(db.Node, *nvkeproto.StartKubernetesCertificateRenewalRequest) (*nvkeproto.StartKubernetesCertificateRenewalResponse, error) {
			require.NotNil(t, store.cluster.CertificateRenewal, "the reservation is held while the RPC runs")
			return nil, errors.New("connection refused")
		},
	}
	a := fenceActivities(store, agent)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(a.RenewKubernetesCertificates)

	_, err := env.ExecuteActivity(a.RenewKubernetesCertificates, activities.KubernetesCertificateRenewalInput{
		NodeID: store.nodes[0].ID, OperationID: "v1-operation", ThresholdDays: 14,
	})

	require.ErrorContains(t, err, "failed to start certificate renewal")
	require.Nil(t, store.cluster.CertificateRenewal, "a failed V1 Start must not outlive its activity")
	require.NoError(t, beginFenceOwner(t, a, store, db.OperationTypeCertRenewal), "a later V2 cycle is admitted")
	require.Equal(t, db.CertificateRenewalStateAccepted, store.cluster.Conditions.CertificateRenewal.State)
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

func TestRotationPreservesWorkerAdmissionsAndControlPlaneObservations(t *testing.T) {
	store := newFenceStore()
	store.cluster = runningRotation()
	store.nodes = []db.Node{stableRenewalNode(store.cluster.ID)}
	a := fenceActivities(store, nil)
	require.NoError(t, a.UpdateNodeConditions(t.Context(), activities.NodeConditionsUpdate{
		NodeID:     store.nodes[0].ID,
		Conditions: activities.NodeConditions{NodeAgentReady: &activities.ConditionUpdate{Status: db.ConditionStatusFalse, Message: "unreachable"}},
	}))
	require.Equal(t, db.EtcdKeyRotationStateRunning, store.cluster.Conditions.EtcdKeyRotation.State)
	require.Equal(t, db.OperationTypeEtcdKeyRotation, store.cluster.Conditions.UpdateInProgress.Type)
	require.Equal(t, []string{"node"}, store.lockOrder)
	store.nodes[0].ControlPlane = false
	require.NoError(t, a.UpdateNodeConditions(t.Context(), activities.NodeConditionsUpdate{
		NodeID:     store.nodes[0].ID,
		Conditions: activities.NodeConditions{Provisioning: &activities.ConditionUpdate{Status: db.ConditionStatusTrue, Message: "worker join"}},
	}))
	require.Equal(t, []string{"node"}, store.lockOrder)
	require.NoError(t, a.RecordEtcdKeyRotationPhase(t.Context(), activities.RecordEtcdKeyRotationPhaseInput{ClusterID: store.cluster.ID, WorkflowID: "owner", Phase: db.EtcdKeyRotationPhaseRewrite}))
	require.Equal(t, db.EtcdKeyRotationPhaseRewrite, store.cluster.Conditions.EtcdKeyRotation.Phase)
}

func TestRotationStopsWhenClusterDeletionStarts(t *testing.T) {
	store := newFenceStore()
	store.cluster = runningRotation()
	store.cluster.Conditions.Deleting = db.NewCondition(true, "Tearing down")
	store.nodes = []db.Node{stableRenewalNode(store.cluster.ID)}
	a := fenceActivities(store, nil)
	err := a.RecordEtcdKeyRotationPhase(t.Context(), activities.RecordEtcdKeyRotationPhaseInput{ClusterID: store.cluster.ID, WorkflowID: "owner", Phase: db.EtcdKeyRotationPhaseRewrite})
	require.ErrorContains(t, err, "cluster deletion is in progress")
	err = a.ValidateEtcdRotationMembership(t.Context(), activities.ValidateEtcdRotationMembershipInput{ClusterID: store.cluster.ID, WorkflowID: "owner", ExpectedNodeIDs: []uuid.UUID{store.nodes[0].ID}})
	require.ErrorContains(t, err, "no longer owns")
	require.Zero(t, store.writes)
}

func TestRotationRemovalGateRejectsMembershipDriftAndRequiresEveryNodeReady(t *testing.T) {
	for _, failure := range []string{"", "db_membership", "unfinished", "agent_not_ready", "api_not_ready", "live_membership"} {
		name := failure
		if name == "" {
			name = "converged"
		}
		t.Run(name, func(t *testing.T) {
			store := newFenceStore()
			store.cluster = runningRotation()
			store.nodes = []db.Node{stableRenewalNode(store.cluster.ID)}
			store.nodes = append(store.nodes, stableRenewalNode(store.cluster.ID))
			store.nodes[1].Name = "cp-1"
			ids := []uuid.UUID{store.nodes[0].ID, store.nodes[1].ID}
			switch failure {
			case "db_membership":
				store.nodes[1].ID = uuid.New()
			case "unfinished":
				store.nodes[1].Conditions.DeleteRequested = db.NewCondition(true, "")
			case "agent_not_ready":
				store.nodes[1].Conditions.NodeAgentReady = db.NewCondition(false, "")
			}
			agent := &agenttest.FakeClient{ExecuteScriptHook: func(node db.Node, name, _ string) (*nvkeproto.ExecuteScriptResponse, error) {
				if name == "etcd-rotation-membership" {
					if failure == "live_membership" {
						return &nvkeproto.ExecuteScriptResponse{ExitCode: 1, ErrorMessage: "untrusted output"}, nil
					}
					return &nvkeproto.ExecuteScriptResponse{}, nil
				}
				if failure == "api_not_ready" && node.ID == store.nodes[1].ID {
					return nil, errors.New("untrusted output")
				}
				return &nvkeproto.ExecuteScriptResponse{Output: "[+]ping ok\n[+]etcd ok\n"}, nil
			}}
			err := fenceActivities(store, agent).ValidateEtcdRotationMembership(t.Context(), activities.ValidateEtcdRotationMembershipInput{ClusterID: store.cluster.ID, WorkflowID: "owner", ExpectedNodeIDs: ids})
			if failure == "" {
				require.NoError(t, err)
				require.Len(t, agent.ExecuteScriptNames, 3)
			} else {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "untrusted output")
			}
			require.Equal(t, db.OperationTypeEtcdKeyRotation, store.cluster.Conditions.UpdateInProgress.Type)
			require.Zero(t, store.writes)
		})
	}
}
