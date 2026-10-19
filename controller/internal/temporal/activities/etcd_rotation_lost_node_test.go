// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/credentialaudit"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

// lostNodeStore adds the node deletion used by the retired node's teardown.
type lostNodeStore struct {
	*fenceStore
	deleted []uuid.UUID
}

func (s *lostNodeStore) RunInTx(ctx context.Context, fn func(context.Context, db.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lockOrder = nil
	return fn(ctx, s)
}

func (s *lostNodeStore) DeleteNode(_ context.Context, id uuid.UUID) error {
	s.deleted = append(s.deleted, id)
	return nil
}

func lostNodeActivities(store *lostNodeStore, agent *agenttest.FakeClient) *activities.Activities {
	return activities.NewActivities(config.Config{}, store, nil, agent, "", nil, "")
}

// failedRotationWithLostNode models a three-node control plane whose rotation
// failed in phase after the second node became unreachable.
func failedRotationWithLostNode(phase db.EtcdKeyRotationPhase) (*lostNodeStore, db.Node) {
	store := newFenceStore()
	store.cluster = runningRotation()
	store.cluster.Conditions.EtcdKeyRotation.State = db.EtcdKeyRotationStateFailed
	store.cluster.Conditions.EtcdKeyRotation.Phase = phase
	store.nodes = nil
	for range 3 {
		node := stableRenewalNode(store.cluster.ID)
		node.Conditions.KubernetesReady = db.NewCondition(true, "ready")
		store.nodes = append(store.nodes, node)
	}
	lost := &store.nodes[1]
	lost.Conditions.NodeAgentReady = db.NewCondition(false, "unreachable")
	lost.Conditions.KubernetesReady = db.NewCondition(false, "NotReady")
	return &lostNodeStore{fenceStore: store}, *lost
}

func retireInput(store *lostNodeStore, node db.Node) activities.RetireLostEtcdRotationNodeInput {
	return activities.RetireLostEtcdRotationNodeInput{ClusterID: store.cluster.ID, RotationWorkflowID: "owner", NodeID: node.ID}
}

func unreachableAgent() *agenttest.FakeClient {
	return &agenttest.FakeClient{AgentVersionErr: errors.New("connection refused")}
}

func TestRetireLostEtcdRotationNodeRecordsAuditedRemovalAfterNodePhases(t *testing.T) {
	for _, phase := range []db.EtcdKeyRotationPhase{db.EtcdKeyRotationPhaseAddSecondary, db.EtcdKeyRotationPhaseRewrite} {
		t.Run(string(phase), func(t *testing.T) {
			store, lost := failedRotationWithLostNode(phase)
			a := lostNodeActivities(store, unreachableAgent())

			result, err := a.RetireLostEtcdRotationNode(t.Context(), retireInput(store, lost))
			require.NoError(t, err)
			require.True(t, result.TeardownRequired)

			status := store.cluster.Conditions.EtcdKeyRotation
			require.Len(t, status.RetiredNodes, 1)
			require.Equal(t, lost.ID, status.RetiredNodes[0].NodeID)
			require.Equal(t, phase, status.RetiredNodes[0].Phase)
			require.False(t, status.RetiredNodes[0].RetiredAt.IsZero())
			// The lock and the failed state are retained: only completion releases ownership.
			require.Equal(t, db.OperationTypeEtcdKeyRotation, store.cluster.Conditions.UpdateInProgress.Type)
			require.Equal(t, db.EtcdKeyRotationStateFailed, status.State)
			require.Equal(t, db.ConditionStatusTrue, store.nodes[1].Conditions.Deleting.Status)
			require.Equal(t, []string{"cluster", "node"}, store.lockOrder)

			require.Len(t, store.auditEvents, 1)
			event := store.auditEvents[0]
			require.Equal(t, credentialaudit.ActionRotate, event.Action)
			require.Equal(t, credentialaudit.TypeEtcdKey, event.Credential)
			require.Equal(t, credentialaudit.OutcomeSuccess, event.Outcome)
			require.Equal(t, credentialaudit.ReasonNodeRetired, event.Reason)
			require.Equal(t, string(phase), event.Phase)
			require.Equal(t, lost.ID.String(), event.Target.NodeID)
			require.Equal(t, credentialaudit.Hash("operation", "owner"), event.Correlation.OperationHash)

			// Only the retired node may now be removed; every other mutation stays fenced.
			require.False(t, db.ControlPlaneNodeRemovalBlocked(store.cluster, lost.ID))
			require.True(t, db.ControlPlaneNodeRemovalBlocked(store.cluster, store.nodes[0].ID))
			require.True(t, db.ControlPlaneMutationBlocked(store.cluster))
		})
	}
}

func TestRetireLostEtcdRotationNodeIsIdempotent(t *testing.T) {
	store, lost := failedRotationWithLostNode(db.EtcdKeyRotationPhaseRewrite)
	a := lostNodeActivities(store, unreachableAgent())
	_, err := a.RetireLostEtcdRotationNode(t.Context(), retireInput(store, lost))
	require.NoError(t, err)
	first := store.cluster.Conditions.EtcdKeyRotation.RetiredNodes[0].RetiredAt
	writes := store.writes

	// A repeat still requests teardown of a remaining row, and stops once
	// teardown has removed it. Neither repeat writes state again.
	result, err := a.RetireLostEtcdRotationNode(t.Context(), retireInput(store, lost))
	require.NoError(t, err)
	require.True(t, result.TeardownRequired)
	store.nodes = append(store.nodes[:1], store.nodes[2:]...)
	result, err = a.RetireLostEtcdRotationNode(t.Context(), retireInput(store, lost))
	require.NoError(t, err)
	require.False(t, result.TeardownRequired)

	require.Equal(t, writes, store.writes)
	require.Len(t, store.cluster.Conditions.EtcdKeyRotation.RetiredNodes, 1)
	require.Equal(t, first, store.cluster.Conditions.EtcdKeyRotation.RetiredNodes[0].RetiredAt)
	require.Len(t, store.auditEvents, 2) // the second repeat is the same semantic no-op event
	require.Equal(t, credentialaudit.OutcomeNoop, store.auditEvents[1].Outcome)
	require.Equal(t, credentialaudit.ReasonUnchanged, store.auditEvents[1].Reason)
}

func TestRetireLostEtcdRotationNodeRefusesUnsafeRequests(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*lostNodeStore, *db.Node, *activities.RetireLostEtcdRotationNodeInput, *agenttest.FakeClient)
		want   string
	}{
		{"wrong owner", func(_ *lostNodeStore, _ *db.Node, in *activities.RetireLostEtcdRotationNodeInput, _ *agenttest.FakeClient) {
			in.RotationWorkflowID = "other"
		}, "does not own"},
		{"lock released", func(s *lostNodeStore, _ *db.Node, _ *activities.RetireLostEtcdRotationNodeInput, _ *agenttest.FakeClient) {
			s.cluster.Conditions.UpdateInProgress = nil
		}, "does not own"},
		{"rotation still running", func(s *lostNodeStore, _ *db.Node, _ *activities.RetireLostEtcdRotationNodeInput, _ *agenttest.FakeClient) {
			s.cluster.Conditions.EtcdKeyRotation.State = db.EtcdKeyRotationStateRunning
		}, "must have failed"},
		{"wrong phase", func(s *lostNodeStore, _ *db.Node, _ *activities.RetireLostEtcdRotationNodeInput, _ *agenttest.FakeClient) {
			s.cluster.Conditions.EtcdKeyRotation.Phase = db.EtcdKeyRotationPhaseFinalize
		}, "phase"},
		{"cluster deleting", func(s *lostNodeStore, _ *db.Node, _ *activities.RetireLostEtcdRotationNodeInput, _ *agenttest.FakeClient) {
			s.cluster.Conditions.Deleting = db.NewCondition(true, "")
		}, "deletion"},
		{"agent ready in database", func(s *lostNodeStore, _ *db.Node, _ *activities.RetireLostEtcdRotationNodeInput, _ *agenttest.FakeClient) {
			s.nodes[1].Conditions.NodeAgentReady = db.NewCondition(true, "ready")
		}, "not lost"},
		{"kubelet ready in database", func(s *lostNodeStore, _ *db.Node, _ *activities.RetireLostEtcdRotationNodeInput, _ *agenttest.FakeClient) {
			s.nodes[1].Conditions.KubernetesReady = db.NewCondition(true, "ready")
		}, "not lost"},
		{"agent answers live probe", func(_ *lostNodeStore, _ *db.Node, _ *activities.RetireLostEtcdRotationNodeInput, agent *agenttest.FakeClient) {
			agent.AgentVersionErr = nil
		}, "not lost"},
		{"worker node", func(s *lostNodeStore, _ *db.Node, _ *activities.RetireLostEtcdRotationNodeInput, _ *agenttest.FakeClient) {
			s.nodes[1].ControlPlane = false
		}, "control-plane node"},
		{"node of another cluster", func(s *lostNodeStore, _ *db.Node, _ *activities.RetireLostEtcdRotationNodeInput, _ *agenttest.FakeClient) {
			s.nodes[1].ClusterID = uuid.New()
		}, "control-plane node"},
		{"never joined", func(s *lostNodeStore, _ *db.Node, _ *activities.RetireLostEtcdRotationNodeInput, _ *agenttest.FakeClient) {
			s.nodes[1].Conditions.Joined = db.NewCondition(false, "")
		}, "control-plane node"},
		{"unknown node", func(_ *lostNodeStore, _ *db.Node, in *activities.RetireLostEtcdRotationNodeInput, _ *agenttest.FakeClient) {
			in.NodeID = uuid.New()
		}, "control-plane node"},
		{"another removal in flight", func(s *lostNodeStore, _ *db.Node, _ *activities.RetireLostEtcdRotationNodeInput, _ *agenttest.FakeClient) {
			s.nodes[2].Conditions.Deleting = db.NewCondition(true, "")
		}, "another control-plane"},
		{"no ready survivor quorum", func(s *lostNodeStore, _ *db.Node, _ *activities.RetireLostEtcdRotationNodeInput, _ *agenttest.FakeClient) {
			s.nodes[2].Conditions.NodeAgentReady = db.NewCondition(false, "unreachable")
		}, "quorum"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, lost := failedRotationWithLostNode(db.EtcdKeyRotationPhaseRewrite)
			agent := unreachableAgent()
			input := retireInput(store, lost)
			tt.mutate(store, &lost, &input, agent)
			a := lostNodeActivities(store, agent)

			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivity(a.RetireLostEtcdRotationNode)
			_, err := env.ExecuteActivity(a.RetireLostEtcdRotationNode, input)
			require.ErrorContains(t, err, tt.want)
			var appErr *temporal.ApplicationError
			require.ErrorAs(t, err, &appErr)
			require.True(t, appErr.NonRetryable())
			require.Zero(t, store.writes)
			require.Empty(t, store.cluster.Conditions.EtcdKeyRotation.RetiredNodes)
			require.Len(t, store.auditEvents, 1)
			require.Equal(t, credentialaudit.OutcomeFailure, store.auditEvents[0].Outcome)
		})
	}
}

// TestRetiredNodeTeardownActivitiesPassFenceOnlyForThatNode exercises the
// TeardownNode activities that check ownership; the survivors stay fenced.
func TestRetiredNodeTeardownActivitiesPassFenceOnlyForThatNode(t *testing.T) {
	store, lost := failedRotationWithLostNode(db.EtcdKeyRotationPhaseRewrite)
	agent := unreachableAgent()
	a := lostNodeActivities(store, agent)
	_, err := a.RetireLostEtcdRotationNode(t.Context(), retireInput(store, lost))
	require.NoError(t, err)
	survivor := store.nodes[0]

	deleting := activities.NodeConditions{Deleting: &activities.ConditionUpdate{Status: db.ConditionStatusTrue, Message: "Removing etcd member"}}
	require.NoError(t, a.UpdateNodeConditions(t.Context(), activities.NodeConditionsUpdate{NodeID: lost.ID, Conditions: deleting}))
	require.ErrorIs(t, a.UpdateNodeConditions(t.Context(), activities.NodeConditionsUpdate{NodeID: survivor.ID, Conditions: deleting}),
		db.ErrControlPlaneMutationBlocked)
	provisioning := activities.NodeConditions{Provisioning: &activities.ConditionUpdate{Status: db.ConditionStatusTrue, Message: "join"}}
	require.ErrorIs(t, a.UpdateNodeConditions(t.Context(), activities.NodeConditionsUpdate{NodeID: lost.ID, Conditions: provisioning}),
		db.ErrControlPlaneMutationBlocked)

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(a.RemoveEtcdMember)
	_, err = env.ExecuteActivity(a.RemoveEtcdMember, activities.EtcdMemberRemovalInput{
		ClusterID: store.cluster.ID, TargetNodeID: lost.ID, TargetMemberName: lost.Name,
	})
	require.NoError(t, err)
	require.Len(t, agent.RemoveEtcdMemberCalls, 1)
	require.NotEqual(t, lost.ID, agent.RemoveEtcdMemberCalls[0])
	_, err = env.ExecuteActivity(a.RemoveEtcdMember, activities.EtcdMemberRemovalInput{
		ClusterID: store.cluster.ID, TargetNodeID: survivor.ID, TargetMemberName: survivor.Name,
	})
	require.ErrorContains(t, err, db.ErrControlPlaneMutationBlocked.Error())

	require.NoError(t, a.DeleteNode(t.Context(), lost.ID))
	require.Equal(t, []uuid.UUID{lost.ID}, store.deleted)
	require.ErrorIs(t, a.DeleteNode(t.Context(), survivor.ID), db.ErrControlPlaneMutationBlocked)
	require.Equal(t, []uuid.UUID{lost.ID}, store.deleted)
}

// commitFailingLostNodeStore runs the transaction body, then fails the commit.
type commitFailingLostNodeStore struct {
	*lostNodeStore
}

func (s *commitFailingLostNodeStore) RunInTx(ctx context.Context, fn func(context.Context, db.Tx) error) error {
	if err := s.lostNodeStore.RunInTx(ctx, fn); err != nil {
		return err
	}
	return errors.New("commit failed")
}

// TestRetireLostEtcdRotationNodeAuditsFailedCommitAsFailure keeps a rolled-back
// retirement from being audited as rotate/success/node_retired.
func TestRetireLostEtcdRotationNodeAuditsFailedCommitAsFailure(t *testing.T) {
	store, lost := failedRotationWithLostNode(db.EtcdKeyRotationPhaseRewrite)
	failing := &commitFailingLostNodeStore{lostNodeStore: store}
	a := activities.NewActivities(config.Config{}, failing, nil, unreachableAgent(), "", nil, "")

	_, err := a.RetireLostEtcdRotationNode(t.Context(), retireInput(store, lost))

	require.ErrorContains(t, err, "commit failed")
	require.Len(t, store.auditEvents, 1)
	require.Equal(t, credentialaudit.OutcomeFailure, store.auditEvents[0].Outcome)
	require.Equal(t, credentialaudit.ReasonFailed, store.auditEvents[0].Reason)
}
