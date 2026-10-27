// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

// cleanupAgent observes both actual RPC boundaries, including uncertain results.
type cleanupAgent struct {
	agenttest.FakeClient
	resetCalls int
	resetError error
}

func (a *cleanupAgent) ExecuteKubeadmReset(context.Context, db.Node, *nvkeproto.KubeadmResetRequest) (*nvkeproto.KubeadmResetStatusResponse, error) {
	a.resetCalls++
	return &nvkeproto.KubeadmResetStatusResponse{}, a.resetError
}

func preJoinCleanupStore() *fenceStore {
	s := newFenceStore()
	s.cluster.Conditions.UpdateInProgress = nil
	target := stableRenewalNode(s.cluster.ID)
	target.Name = "cp-joining"
	target.Conditions.Joined = db.NewCondition(false, "not joined")
	target.Conditions.Provisioned = db.NewCondition(false, "not provisioned")
	target.Conditions.ReadyToJoin = db.NewCondition(true, "host ready")
	// A third stable voter keeps renewal admissible once the target has joined:
	// two control-plane voters are refused regardless of cleanup.
	peer := stableRenewalNode(s.cluster.ID)
	peer.Name = "cp-1"
	s.nodes = append(s.nodes, target, peer)
	return s
}

func runCleanupActivity(t *testing.T, a *activities.Activities, target db.Node, operation string) error {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	if operation == "remove_member" {
		env.RegisterActivity(a.RemoveEtcdMember)
		_, err := env.ExecuteActivity(a.RemoveEtcdMember, activities.EtcdMemberRemovalInput{
			ClusterID: target.ClusterID, TargetNodeID: target.ID, TargetMemberName: target.Name,
		})
		return err
	}
	env.RegisterActivity(a.KubeadmReset)
	_, err := env.ExecuteActivity(a.KubeadmReset, activities.KubeadmResetInput{
		NodeID: target.ID, SkipPhases: []string{"remove-etcd-member"},
	})
	return err
}

func TestPreJoinCleanupRetainsProvisioning(t *testing.T) {
	for _, owner := range fenceOwners {
		t.Run(string(owner), func(t *testing.T) {
			for _, retry := range []bool{false, true} {
				t.Run(map[bool]string{false: "fresh", true: "retry_after_uncertain_cleanup"}[retry], func(t *testing.T) {
					store := preJoinCleanupStore()
					agent := &cleanupAgent{}
					a := activities.NewActivities(config.Config{}, store, nil, agent, "", nil, "")
					if retry {
						store.nodes[1].Conditions.Provisioning = db.NewCondition(true, "earlier join failed")
						agent.RemoveEtcdMemberError = errors.New("RPC outcome unknown")
						agent.resetError = errors.New("RPC outcome unknown")
						for _, op := range []string{"remove_member", "reset"} {
							require.ErrorContains(t, runCleanupActivity(t, a, store.nodes[1], op), "RPC outcome unknown")
							require.Equal(t, db.ConditionStatusTrue, store.nodes[1].Conditions.Provisioning.Status)
							require.NotEqual(t, db.ConditionStatusTrue, store.nodes[1].Conditions.Deleting.Status)
						}
						agent.RemoveEtcdMemberError, agent.resetError = nil, nil
					}
					for _, op := range []string{"remove_member", "reset"} {
						require.NoError(t, runCleanupActivity(t, a, store.nodes[1], op))
						require.Equal(t, []string{"cluster", "node"}, store.lockOrder)
						require.Equal(t, db.ConditionStatusTrue, store.nodes[1].Conditions.Provisioning.Status)
						require.NotEqual(t, db.ConditionStatusTrue, store.nodes[1].Conditions.Deleting.Status)
						require.Error(t, beginFenceOwner(t, a, store, owner))
					}
					wantCalls := 1
					if retry {
						wantCalls = 2
					}
					require.Len(t, agent.RemoveEtcdMemberCalls, wantCalls)
					require.Equal(t, wantCalls, agent.resetCalls)
					// The normal join-completion condition updates must suffice; no
					// invented Deleting cleanup is allowed to make admission pass.
					require.NoError(t, a.UpdateNodeConditions(t.Context(), activities.NodeConditionsUpdate{
						NodeID: store.nodes[1].ID, Conditions: activities.NodeConditions{
							Joined: &activities.ConditionUpdate{Status: db.ConditionStatusTrue},
						},
					}))
					require.Error(t, beginFenceOwner(t, a, store, owner), "post-join provisioning still owns the node")
					require.NoError(t, a.UpdateNodeConditions(t.Context(), activities.NodeConditionsUpdate{
						NodeID: store.nodes[1].ID, Conditions: activities.NodeConditions{
							Provisioning: &activities.ConditionUpdate{Status: db.ConditionStatusFalse},
							Provisioned:  &activities.ConditionUpdate{Status: db.ConditionStatusTrue},
						},
					}))
					require.NoError(t, beginFenceOwner(t, a, store, owner))
				})
			}
		})
	}
}

func TestControlPlaneCleanupAndCredentialsAreExclusive(t *testing.T) {
	for _, owner := range fenceOwners {
		t.Run(string(owner), func(t *testing.T) {
			for _, op := range []string{"remove_member", "reset"} {
				for _, credentialFirst := range []bool{false, true} {
					t.Run(op+map[bool]string{false: "/cleanup_first", true: "/credential_first"}[credentialFirst], func(t *testing.T) {
						store := preJoinCleanupStore()
						agent := &cleanupAgent{}
						a := activities.NewActivities(config.Config{}, store, nil, agent, "", nil, "")
						if credentialFirst {
							// Admit on a stable control plane, then deliver old cleanup
							// work. No remote mutation or node write may escape the owner.
							store.nodes[1].Conditions.Joined = db.NewCondition(true, "joined")
							store.nodes[1].Conditions.Provisioned = db.NewCondition(true, "provisioned")
							require.NoError(t, beginFenceOwner(t, a, store, owner))
							writes := store.writes
							require.ErrorContains(t, runCleanupActivity(t, a, store.nodes[1], op), db.ErrControlPlaneMutationBlocked.Error())
							require.Equal(t, []string{"cluster"}, store.lockOrder)
							require.Equal(t, writes, store.writes)
							require.Empty(t, agent.RemoveEtcdMemberCalls)
							require.Zero(t, agent.resetCalls)
						} else {
							require.NoError(t, runCleanupActivity(t, a, store.nodes[1], op))
							require.Equal(t, []string{"cluster", "node"}, store.lockOrder)
							// Even if Joined/Provisioned observations catch up before
							// completion, the durable provisioning reservation fences Begin.
							store.nodes[1].Conditions.Joined = db.NewCondition(true, "joined")
							store.nodes[1].Conditions.Provisioned = db.NewCondition(true, "observed")
							require.Equal(t, db.ConditionStatusTrue, store.nodes[1].Conditions.Provisioning.Status)
							require.Error(t, beginFenceOwner(t, a, store, owner))
							require.Nil(t, store.cluster.Conditions.UpdateInProgress)
						}
					})
				}
			}
		})
	}
}

// The activity's unlocked lookup is deliberately stale; the transaction must
// classify the current lifecycle from GetNodeForUpdate, in either direction.
type staleCleanupStore struct {
	*fenceStore
	snapshot db.Node
}

func (s *staleCleanupStore) GetNode(context.Context, uuid.UUID) (db.Node, error) {
	return s.snapshot, nil
}

func TestControlPlaneCleanupReadsLockedLifecycle(t *testing.T) {
	for _, owner := range fenceOwners {
		t.Run(string(owner), func(t *testing.T) {
			for _, op := range []string{"remove_member", "reset"} {
				for _, state := range []string{"pre_join", "joined", "delete_requested", "deleting", "requested_and_deleting", "stale_deleting"} {
					for _, uncertain := range []bool{false, true} {
						t.Run(op+"/"+state+map[bool]string{false: "/success", true: "/unknown"}[uncertain], func(t *testing.T) {
							store := preJoinCleanupStore()
							snapshot := store.nodes[1]
							switch state {
							case "pre_join":
								snapshot.Conditions.Joined = db.NewCondition(true, "stale")
							case "joined":
								store.nodes[1].Conditions.Joined = db.NewCondition(true, "joined")
							case "delete_requested":
								store.nodes[1].Conditions.DeleteRequested = db.NewCondition(true, "accepted")
							case "deleting", "requested_and_deleting":
								store.nodes[1].Conditions.Deleting = db.NewCondition(true, "teardown")
								if state == "requested_and_deleting" {
									store.nodes[1].Conditions.DeleteRequested = db.NewCondition(true, "accepted")
								}
							case "stale_deleting":
								snapshot.Conditions.Deleting = db.NewCondition(true, "stale")
								store.nodes[1].Conditions.DeleteRequested = db.NewCondition(true, "accepted")
							}
							requested := store.nodes[1].Conditions.DeleteRequested
							agent := &cleanupAgent{}
							if uncertain {
								agent.RemoveEtcdMemberError = errors.New("RPC outcome unknown")
								agent.resetError = errors.New("RPC outcome unknown")
							}
							a := activities.NewActivities(config.Config{}, &staleCleanupStore{store, snapshot}, nil, agent, "", nil, "")
							err := runCleanupActivity(t, a, snapshot, op)
							if uncertain {
								require.ErrorContains(t, err, "RPC outcome unknown")
							} else {
								require.NoError(t, err)
							}
							require.Equal(t, []string{"cluster", "node"}, store.lockOrder)
							require.Equal(t, requested, store.nodes[1].Conditions.DeleteRequested, "cleanup must not promote or erase queued intent")
							if state == "pre_join" || state == "delete_requested" || state == "stale_deleting" {
								require.NotEqual(t, db.ConditionStatusTrue, store.nodes[1].Conditions.Deleting.Status)
								require.Equal(t, db.ConditionStatusTrue, store.nodes[1].Conditions.Provisioning.Status)
							} else {
								require.Equal(t, db.ConditionStatusTrue, store.nodes[1].Conditions.Deleting.Status)
							}
							require.Error(t, beginFenceOwner(t, a, store, owner))
						})
					}
				}
			}
		})
	}
}

func TestCAPIMachineDeletionReservesTeardownEvenBeforeJoin(t *testing.T) {
	for _, owner := range fenceOwners {
		t.Run(string(owner), func(t *testing.T) {
			store := preJoinCleanupStore()
			scheme := runtime.NewScheme()
			require.NoError(t, clusterv1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			a := activities.NewActivities(config.Config{}, store, fake.NewClientBuilder().WithScheme(scheme).Build(), nil, "", nil, "")
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivity(a.DeleteCAPIMachineForNode)
			_, err := env.ExecuteActivity(a.DeleteCAPIMachineForNode, activities.DeleteCAPIMachineForNodeInput{NodeID: store.nodes[1].ID})
			require.NoError(t, err)
			require.Equal(t, []string{"cluster", "node"}, store.lockOrder)
			require.Equal(t, db.ConditionStatusTrue, store.nodes[1].Conditions.Deleting.Status)
			require.Error(t, beginFenceOwner(t, a, store, owner))
		})
	}
}
