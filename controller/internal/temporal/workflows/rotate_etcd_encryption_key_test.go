// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
	rolloutetcd "github.com/dsx-ai-factory/nvke/controller/internal/rollout/operations/etcd"
	rollouttemporal "github.com/dsx-ai-factory/nvke/controller/internal/rollout/temporal"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestRotateEtcdEncryptionKeyWorkflow_CompletesThroughUnguardedRolloutExecutor(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	nodeID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "cp-0", IP: "10.0.0.10", ControlPlane: true}

	env, a := setupEnableEtcdEncryptionWorkflowEnv(t)
	env.OnGetVersion(etcdKeyRotationLifecycleChangeID, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion).Once()
	env.OnActivity(
		a.BeginEtcdEncryptionKeyRotation,
		mock.Anything,
		activities.BeginEtcdEncryptionKeyRotationInput{ClusterID: clusterID},
	).Return(activities.BeginEtcdEncryptionKeyRotationResult{}, nil).Once()
	env.OnActivity(
		a.PrepareEtcdEncryptionKeyRotation,
		mock.Anything,
		activities.PrepareEtcdEncryptionKeyRotationInput{ClusterID: clusterID},
	).Return(activities.PrepareEtcdEncryptionResult{
		NodeIDs:        []uuid.UUID{nodeID},
		ExecutorNodeID: nodeID,
	}, nil).Once()
	env.OnActivity(
		a.ValidateEtcdEncryptionHealth,
		mock.Anything,
		activities.ValidateEtcdEncryptionHealthInput{ClusterID: clusterID, ExpectedNodeIDs: []uuid.UUID{nodeID}},
	).Return(nil).Once()
	env.OnActivity(
		rollouttemporal.ActivityLoadClusterForRollout,
		mock.Anything,
		&rollouttemporal.LoadClusterForRolloutRequest{ClusterID: clusterID},
	).Return(&rollouttemporal.LoadClusterForRolloutResponse{
		Cluster: db.Cluster{
			ID:                clusterID,
			Name:              "tenant-a",
			KubernetesVersion: "v1.31.9",
			ControlPlaneVIP:   "10.0.0.1",
		},
		Nodes:         []db.Node{node},
		CPNodes:       []db.Node{node},
		NodeAgentPort: "50051",
	}, nil).Once()
	env.OnActivity(
		a.EnsurePendingEtcdEncryptionRotationKey,
		mock.Anything,
		activities.EtcdEncryptionClusterInput{ClusterID: clusterID},
	).Return(nil).Once()

	addSecondaryCall := expectEtcdRolloutPhase(
		t,
		env,
		clusterID,
		rolloutetcd.OperationNameAddSecondaryEncryptionKey,
		false,
		rollout.OutcomeApplied,
	)
	promotePrimaryCall := expectEtcdRolloutPhase(
		t,
		env,
		clusterID,
		rolloutetcd.OperationNamePromotePrimaryEncryptionKey,
		false,
		rollout.OutcomeApplied,
	)
	rewriteCall := env.OnActivity(a.StartEtcdEncryptionRewrite, mock.Anything, mock.Anything).
		Return(func(_ context.Context, input activities.EtcdEncryptionRewriteInput) (activities.EtcdEncryptionRewriteOperation, error) {
			return activities.EtcdEncryptionRewriteOperation{
				OperationID: input.OperationID,
				Phase:       activities.EtcdEncryptionOperationPhaseSucceeded,
			}, nil
		}).Once()
	removeOldKeysCall := expectEtcdRolloutPhase(
		t,
		env,
		clusterID,
		rolloutetcd.OperationNameRemoveOldEncryptionKeys,
		false,
		rollout.OutcomeApplied,
	)
	finalizeCall := env.OnActivity(
		a.FinalizePendingEtcdEncryptionKey,
		mock.Anything,
		activities.EtcdEncryptionClusterInput{ClusterID: clusterID},
	).Return(nil).Once()
	completeCall := env.OnActivity(
		a.CompleteEtcdEncryptionKeyRotation,
		mock.Anything,
		activities.CompleteEtcdEncryptionKeyRotationInput{ClusterID: clusterID},
	).Return(activities.CompleteEtcdEncryptionKeyRotationResult{}, nil).Once()
	env.InOrderMockCalls(
		addSecondaryCall,
		promotePrimaryCall,
		rewriteCall,
		removeOldKeysCall,
		finalizeCall,
		completeCall,
	)

	env.ExecuteWorkflow(RotateEtcdEncryptionKey, RotateEtcdEncryptionKeyInput{ClusterID: clusterID})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestRotateEtcdEncryptionKeyWorkflow_ClearsProgressAfterExecutorFailure(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	nodeID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "cp-0", IP: "10.0.0.10", ControlPlane: true}

	env, a := setupEnableEtcdEncryptionWorkflowEnv(t)
	env.OnGetVersion(etcdKeyRotationLifecycleChangeID, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion).Once()
	env.OnActivity(
		a.BeginEtcdEncryptionKeyRotation,
		mock.Anything,
		activities.BeginEtcdEncryptionKeyRotationInput{ClusterID: clusterID},
	).Return(activities.BeginEtcdEncryptionKeyRotationResult{}, nil).Once()
	env.OnActivity(
		a.PrepareEtcdEncryptionKeyRotation,
		mock.Anything,
		activities.PrepareEtcdEncryptionKeyRotationInput{ClusterID: clusterID},
	).Return(activities.PrepareEtcdEncryptionResult{
		NodeIDs:        []uuid.UUID{nodeID},
		ExecutorNodeID: nodeID,
	}, nil).Once()
	env.OnActivity(a.ValidateEtcdEncryptionHealth, mock.Anything, mock.Anything).Return(nil).Once()
	env.OnActivity(
		rollouttemporal.ActivityLoadClusterForRollout,
		mock.Anything,
		&rollouttemporal.LoadClusterForRolloutRequest{ClusterID: clusterID},
	).Return(&rollouttemporal.LoadClusterForRolloutResponse{
		Cluster:       db.Cluster{ID: clusterID, Name: "tenant-a"},
		Nodes:         []db.Node{node},
		CPNodes:       []db.Node{node},
		NodeAgentPort: "50051",
	}, nil).Once()
	env.OnActivity(a.EnsurePendingEtcdEncryptionRotationKey, mock.Anything, mock.Anything).Return(nil).Once()
	expectEtcdRolloutPhase(
		t,
		env,
		clusterID,
		rolloutetcd.OperationNameAddSecondaryEncryptionKey,
		false,
		rollout.OutcomeFailedNotRestored,
	)
	env.OnActivity(
		a.ClearEtcdEncryptionUpdateInProgress,
		mock.Anything,
		mock.MatchedBy(func(input activities.ClearEtcdEncryptionUpdateInProgressInput) bool {
			return input.ClusterID == clusterID && input.FailureMessage == etcdEncryptionKeyRotationFailureMessage
		}),
	).Return(activities.ClearEtcdEncryptionUpdateInProgressResult{}, nil).Once()

	env.ExecuteWorkflow(RotateEtcdEncryptionKey, RotateEtcdEncryptionKeyInput{ClusterID: clusterID})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestRotateEtcdEncryptionKeyWorkflow_DoesNotClearAnotherOperationWhenBeginFails(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	env, a := setupEnableEtcdEncryptionWorkflowEnv(t)
	env.OnGetVersion(etcdKeyRotationLifecycleChangeID, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion).Once()
	env.OnActivity(
		a.BeginEtcdEncryptionKeyRotation,
		mock.Anything,
		activities.BeginEtcdEncryptionKeyRotationInput{ClusterID: clusterID},
	).Return(activities.BeginEtcdEncryptionKeyRotationResult{}, errors.New("k8s_upgrade in progress")).Once()

	env.ExecuteWorkflow(RotateEtcdEncryptionKey, RotateEtcdEncryptionKeyInput{ClusterID: clusterID})

	require.True(t, env.IsWorkflowCompleted())
	require.ErrorContains(t, env.GetWorkflowError(), "failed to begin etcd encryption key rotation")
	env.AssertExpectations(t)
}

// TestRotationLifecycleRecordsEachPhaseAndRetainsFailureOwnership exercises new
// executions separately from the legacy command sequence tests above.
func TestRotationLifecycleRecordsEachPhaseAndRetainsFailureOwnership(t *testing.T) {
	for _, failure := range []string{"", "prepare", "add_secondary", "promote_primary", "rewrite", "remove_old_keys", "finalize", "completion"} {
		name := failure
		if name == "" {
			name = "success"
		}
		t.Run(name, func(t *testing.T) {
			clusterID, nodeID := uuid.New(), uuid.New()
			node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "cp-0", IP: "10.0.0.10", ControlPlane: true}
			env, a := setupEnableEtcdEncryptionWorkflowEnv(t)
			env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: "owner"})
			env.OnGetVersion(etcdKeyRotationLifecycleChangeID, workflow.DefaultVersion, 1).Return(workflow.Version(1)).Once()
			env.OnActivity(a.BeginEtcdEncryptionKeyRotation, mock.Anything,
				activities.BeginEtcdEncryptionKeyRotationInput{ClusterID: clusterID, WorkflowID: "owner"},
			).Return(activities.BeginEtcdEncryptionKeyRotationResult{}, nil).Once()
			var prepareErr error
			if failure == "prepare" {
				prepareErr = temporal.NewNonRetryableApplicationError("prepare unavailable", "qualification", nil)
			}
			env.OnActivity(a.PrepareEtcdEncryptionKeyRotation, mock.Anything, mock.Anything).
				Return(activities.PrepareEtcdEncryptionResult{NodeIDs: []uuid.UUID{nodeID}, ExecutorNodeID: nodeID}, prepareErr).Once()
			if failure != "prepare" {
				env.OnActivity(a.ValidateEtcdEncryptionHealth, mock.Anything, mock.Anything).Return(nil).Once()
				env.OnActivity(rollouttemporal.ActivityLoadClusterForRollout, mock.Anything, mock.Anything).
					Return(&rollouttemporal.LoadClusterForRolloutResponse{Cluster: db.Cluster{ID: clusterID, Name: "tenant-a"}, Nodes: []db.Node{node}, CPNodes: []db.Node{node}, NodeAgentPort: "50051"}, nil).Once()
				env.OnActivity(a.EnsurePendingEtcdEncryptionRotationKey, mock.Anything, mock.Anything).Return(nil).Once()
				if failure == "" || failure == "remove_old_keys" || failure == "finalize" || failure == "completion" {
					env.OnGetVersion(etcdKeyRotationMembershipChangeID, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion).Once()
				}
				expectRotationPhases(t, env, a, clusterID, failure)
			}
			if failure != "" {
				env.OnActivity(a.FailEtcdKeyRotation, mock.Anything, activities.FailEtcdKeyRotationInput{
					ClusterID: clusterID, WorkflowID: "owner", Reason: db.EtcdKeyRotationFailureOperation,
				}).Return(nil).Once()
			}
			env.ExecuteWorkflow(RotateEtcdEncryptionKey, RotateEtcdEncryptionKeyInput{ClusterID: clusterID})
			require.True(t, env.IsWorkflowCompleted())
			if failure == "" {
				require.NoError(t, env.GetWorkflowError())
			} else {
				require.Error(t, env.GetWorkflowError())
			}
			env.AssertExpectations(t)
		})
	}
}

func expectRotationPhases(t *testing.T, env *testsuite.TestWorkflowEnvironment, a *activities.Activities, clusterID uuid.UUID, failure string) {
	t.Helper()
	for _, phase := range []db.EtcdKeyRotationPhase{
		db.EtcdKeyRotationPhaseAddSecondary, db.EtcdKeyRotationPhasePromotePrimary,
		db.EtcdKeyRotationPhaseRewrite, db.EtcdKeyRotationPhaseRemoveOldKeys, db.EtcdKeyRotationPhaseFinalize,
	} {
		env.OnActivity(a.RecordEtcdKeyRotationPhase, mock.Anything, activities.RecordEtcdKeyRotationPhaseInput{
			ClusterID: clusterID, WorkflowID: "owner", Phase: phase,
		}).Return(nil).Once()
		failed := failure == string(phase)
		outcome := rollout.OutcomeApplied
		if failed {
			outcome = rollout.OutcomeFailedNotRestored
		}
		switch phase {
		case db.EtcdKeyRotationPhaseAddSecondary:
			expectEtcdRolloutPhase(t, env, clusterID, rolloutetcd.OperationNameAddSecondaryEncryptionKey, false, outcome)
		case db.EtcdKeyRotationPhasePromotePrimary:
			expectEtcdRolloutPhase(t, env, clusterID, rolloutetcd.OperationNamePromotePrimaryEncryptionKey, false, outcome)
		case db.EtcdKeyRotationPhaseRewrite:
			env.OnActivity(a.StartEtcdEncryptionRewrite, mock.Anything, mock.Anything).
				Return(func(_ context.Context, input activities.EtcdEncryptionRewriteInput) (activities.EtcdEncryptionRewriteOperation, error) {
					state := activities.EtcdEncryptionOperationPhaseSucceeded
					if failed {
						state = activities.EtcdEncryptionOperationPhaseFailed
					}
					return activities.EtcdEncryptionRewriteOperation{OperationID: input.OperationID, Phase: state}, nil
				}).Once()
		case db.EtcdKeyRotationPhaseRemoveOldKeys:
			expectEtcdRolloutPhase(t, env, clusterID, rolloutetcd.OperationNameRemoveOldEncryptionKeys, false, outcome)
		case db.EtcdKeyRotationPhaseFinalize:
			var err error
			if failed {
				err = temporal.NewNonRetryableApplicationError("finalization unavailable", "qualification", nil)
			}
			env.OnActivity(a.FinalizeEtcdRotationKey, mock.Anything, mock.Anything).Return(err).Once()
		case db.EtcdKeyRotationPhasePrepare:
			t.Fatal("prepare is handled before rotation phases")
		}
		if failed {
			return
		}
	}
	var err error
	if failure == "completion" {
		err = temporal.NewNonRetryableApplicationError("completion unavailable", "qualification", nil)
	}
	env.OnActivity(a.CompleteEtcdEncryptionKeyRotation, mock.Anything,
		activities.CompleteEtcdEncryptionKeyRotationInput{ClusterID: clusterID, WorkflowID: "owner"},
	).Return(activities.CompleteEtcdEncryptionKeyRotationResult{}, err).Once()
}

func TestRotationRevalidatesMembershipAndPrimaryBeforeOldKeyRemoval(t *testing.T) {
	for _, failure := range []string{"", "membership", "primary_convergence", "final_membership"} {
		name := failure
		if name == "" {
			name = "converged"
		}
		t.Run(name, func(t *testing.T) {
			clusterID, nodeID := uuid.New(), uuid.New()
			node := db.Node{ID: nodeID, ClusterID: clusterID, ControlPlane: true, Name: "cp-0", IP: "10.0.0.10"}
			env, a := setupEnableEtcdEncryptionWorkflowEnv(t)
			env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: "owner"})
			env.OnGetVersion(etcdKeyRotationLifecycleChangeID, workflow.DefaultVersion, 1).Return(workflow.Version(1)).Once()
			env.OnGetVersion(etcdKeyRotationMembershipChangeID, workflow.DefaultVersion, 1).Return(workflow.Version(1)).Once()
			env.OnActivity(a.BeginEtcdEncryptionKeyRotation, mock.Anything, mock.Anything).Return(activities.BeginEtcdEncryptionKeyRotationResult{}, nil).Once()
			env.OnActivity(a.PrepareEtcdEncryptionKeyRotation, mock.Anything, mock.Anything).Return(activities.PrepareEtcdEncryptionResult{NodeIDs: []uuid.UUID{nodeID}, ExecutorNodeID: nodeID}, nil).Once()
			env.OnActivity(a.ValidateEtcdEncryptionHealth, mock.Anything, mock.Anything).Return(nil).Once()
			env.OnActivity(rollouttemporal.ActivityLoadClusterForRollout, mock.Anything, mock.Anything).Return(&rollouttemporal.LoadClusterForRolloutResponse{Cluster: db.Cluster{ID: clusterID, Name: "tenant"}, Nodes: []db.Node{node}, CPNodes: []db.Node{node}, NodeAgentPort: "50051"}, nil).Once()
			env.OnActivity(a.EnsurePendingEtcdEncryptionRotationKey, mock.Anything, mock.Anything).Return(nil).Once()
			for _, phase := range []db.EtcdKeyRotationPhase{db.EtcdKeyRotationPhaseAddSecondary, db.EtcdKeyRotationPhasePromotePrimary, db.EtcdKeyRotationPhaseRewrite} {
				env.OnActivity(a.RecordEtcdKeyRotationPhase, mock.Anything, activities.RecordEtcdKeyRotationPhaseInput{ClusterID: clusterID, WorkflowID: "owner", Phase: phase}).Return(nil).Once()
			}
			expectEtcdRolloutPhase(t, env, clusterID, rolloutetcd.OperationNameAddSecondaryEncryptionKey, false, rollout.OutcomeApplied)
			firstPromotion := expectEtcdRolloutPhase(t, env, clusterID, rolloutetcd.OperationNamePromotePrimaryEncryptionKey, false, rollout.OutcomeApplied)
			rewrite := env.OnActivity(a.StartEtcdEncryptionRewrite, mock.Anything, mock.Anything).Return(activities.EtcdEncryptionRewriteOperation{OperationID: "rewrite", Phase: activities.EtcdEncryptionOperationPhaseSucceeded}, nil).Once()
			gateInput := activities.ValidateEtcdRotationMembershipInput{ClusterID: clusterID, WorkflowID: "owner", ExpectedNodeIDs: []uuid.UUID{nodeID}}
			var gateErr error
			if failure == "membership" {
				gateErr = temporal.NewNonRetryableApplicationError("membership drift", "qualification", nil)
			}
			gate := env.OnActivity(a.ValidateEtcdRotationMembership, mock.Anything, gateInput).Return(gateErr).Once()
			env.InOrderMockCalls(firstPromotion, rewrite, gate)
			if failure != "membership" {
				outcome := rollout.OutcomeApplied
				if failure == "primary_convergence" {
					outcome = rollout.OutcomeFailedNotRestored
				}
				promotion := expectEtcdRolloutPhase(t, env, clusterID, rolloutetcd.OperationNamePromotePrimaryEncryptionKey, false, outcome)
				env.InOrderMockCalls(gate, promotion)
				if failure != "primary_convergence" {
					var lastErr error
					if failure == "final_membership" {
						lastErr = temporal.NewNonRetryableApplicationError("membership drift", "qualification", nil)
					}
					lastGate := env.OnActivity(a.ValidateEtcdRotationMembership, mock.Anything, gateInput).Return(lastErr).Once()
					env.InOrderMockCalls(promotion, lastGate)
					if failure == "" {
						env.OnActivity(a.RecordEtcdKeyRotationPhase, mock.Anything, activities.RecordEtcdKeyRotationPhaseInput{ClusterID: clusterID, WorkflowID: "owner", Phase: db.EtcdKeyRotationPhaseRemoveOldKeys}).Return(nil).Once()
						removal := expectEtcdRolloutPhase(t, env, clusterID, rolloutetcd.OperationNameRemoveOldEncryptionKeys, false, rollout.OutcomeApplied)
						env.InOrderMockCalls(lastGate, removal)
						env.OnActivity(a.RecordEtcdKeyRotationPhase, mock.Anything, activities.RecordEtcdKeyRotationPhaseInput{ClusterID: clusterID, WorkflowID: "owner", Phase: db.EtcdKeyRotationPhaseFinalize}).Return(nil).Once()
						env.OnActivity(a.FinalizeEtcdRotationKey, mock.Anything, mock.Anything).Return(nil).Once()
						env.OnActivity(a.CompleteEtcdEncryptionKeyRotation, mock.Anything, mock.Anything).Return(activities.CompleteEtcdEncryptionKeyRotationResult{}, nil).Once()
					}
				}
			}
			if failure != "" {
				env.OnActivity(a.FailEtcdKeyRotation, mock.Anything, activities.FailEtcdKeyRotationInput{ClusterID: clusterID, WorkflowID: "owner", Reason: db.EtcdKeyRotationFailureOperation}).Return(nil).Once()
			}
			env.ExecuteWorkflow(RotateEtcdEncryptionKey, RotateEtcdEncryptionKeyInput{ClusterID: clusterID})
			if failure == "" {
				require.NoError(t, env.GetWorkflowError())
			} else {
				require.Error(t, env.GetWorkflowError())
				env.AssertNotCalled(t, "RecordEtcdKeyRotationPhase", mock.Anything, activities.RecordEtcdKeyRotationPhaseInput{ClusterID: clusterID, WorkflowID: "owner", Phase: db.EtcdKeyRotationPhaseRemoveOldKeys})
				env.AssertNotCalled(t, "FinalizeEtcdRotationKey", mock.Anything, mock.Anything)
			}
			env.AssertExpectations(t)
		})
	}
}

// TestRotationCancellationWaitsForAcceptedBeginAndRecordsFailure models a committed owner whose Begin
// result is delayed. Cancellation must resolve acceptance, record the owner-fenced cancellation
// failure on a disconnected context and dispatch no node work, leaving the lock retained.
func TestRotationCancellationWaitsForAcceptedBeginAndRecordsFailure(t *testing.T) {
	clusterID := uuid.New()
	env, a := setupEnableEtcdEncryptionWorkflowEnv(t)
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: "owner"})
	env.OnGetVersion(etcdKeyRotationLifecycleChangeID, workflow.DefaultVersion, 1).Return(workflow.Version(1)).Once()
	accepted := false
	var beginTaskToken []byte
	env.OnActivity(a.BeginEtcdEncryptionKeyRotation, mock.Anything,
		activities.BeginEtcdEncryptionKeyRotationInput{ClusterID: clusterID, WorkflowID: "owner"},
	).Return(func(ctx context.Context, _ activities.BeginEtcdEncryptionKeyRotationInput) (activities.BeginEtcdEncryptionKeyRotationResult, error) {
		// The owner commit happened, but the workflow has not received the result.
		accepted = true
		beginTaskToken = activity.GetInfo(ctx).TaskToken
		return activities.BeginEtcdEncryptionKeyRotationResult{}, activity.ErrResultPending
	}).Once()
	env.RegisterDelayedCallback(func() {
		require.True(t, accepted)
		env.CancelWorkflow()
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		require.False(t, env.IsWorkflowCompleted(), "cancellation must wait for acceptance to resolve")
		require.NoError(t, env.CompleteActivity(beginTaskToken, activities.BeginEtcdEncryptionKeyRotationResult{}, nil))
	}, 2*time.Second)
	env.OnActivity(a.FailEtcdKeyRotation, mock.Anything, activities.FailEtcdKeyRotationInput{
		ClusterID: clusterID, WorkflowID: "owner", Reason: db.EtcdKeyRotationFailureCancelled,
	}).Return(func(ctx context.Context, _ activities.FailEtcdKeyRotationInput) error {
		require.NoError(t, ctx.Err(), "failure recording must not inherit workflow cancellation")
		return nil
	}).Once()

	env.ExecuteWorkflow(RotateEtcdEncryptionKey, RotateEtcdEncryptionKeyInput{ClusterID: clusterID})

	require.True(t, env.IsWorkflowCompleted())
	require.True(t, temporal.IsCanceledError(env.GetWorkflowError()), "workflow must return the original cancellation")
	env.AssertNotCalled(t, "PrepareEtcdEncryptionKeyRotation", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "ValidateEtcdEncryptionHealth", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "CompleteEtcdEncryptionKeyRotation", mock.Anything, mock.Anything)
	env.AssertExpectations(t)
}

// TestRotationReusingSucceededWorkflowIDFailsWithoutNodeWork runs the real Begin
// activity against a cluster whose recorded rotation with this workflow ID
// already succeeded: the execution must fail and dispatch nothing after Begin.
func TestRotationReusingSucceededWorkflowIDFailsWithoutNodeWork(t *testing.T) {
	clusterID := uuid.New()
	now := time.Now().UTC()
	tx := &testutil.MockTx{}
	tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, EtcdEncrypted: true, Conditions: db.ClusterConditions{
		Provisioned: db.NewCondition(true, "ready"),
		EtcdKeyRotation: &db.EtcdKeyRotationStatus{
			WorkflowID: "owner", State: db.EtcdKeyRotationStateSucceeded, Phase: db.EtcdKeyRotationPhaseFinalize,
			StartedAt: now, LastProgressAt: now, CompletedAt: &now,
		},
	}}, nil)
	store := &testutil.MockDB{MockTx: tx}
	store.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	env, _ := setupEnableEtcdEncryptionWorkflowEnv(t)
	a := activities.NewActivities(config.Config{}, store, nil, nil, "", nil, "")
	env.RegisterActivity(a.BeginEtcdEncryptionKeyRotation)
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: "owner"})
	var started []string
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		started = append(started, info.ActivityType.Name)
	})

	env.ExecuteWorkflow(RotateEtcdEncryptionKey, RotateEtcdEncryptionKeyInput{ClusterID: clusterID})

	require.True(t, env.IsWorkflowCompleted())
	err := env.GetWorkflowError()
	require.ErrorContains(t, err, "already succeeded")
	require.Equal(t, []string{"BeginEtcdEncryptionKeyRotation"}, started, "a non-retryable Begin failure runs once and dispatches nothing after it")
	tx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
}

// TestRotationBeginAcceptanceReplaysRecordedHistories replays an already-recorded rotation that has no
// begin-acceptance marker and one that records it; both must keep their original command sequence.
func TestRotationBeginAcceptanceReplaysRecordedHistories(t *testing.T) {
	for _, tc := range []struct {
		name             string
		acceptanceMarker bool
	}{
		{name: "history recorded before begin-acceptance versioning"},
		{name: "history with begin-acceptance marker", acceptanceMarker: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replayer := worker.NewWorkflowReplayer()
			replayer.RegisterWorkflow(RotateEtcdEncryptionKey)
			require.NoError(t, replayer.ReplayWorkflowHistory(nil, rotationAlreadyCompletedHistory(t, tc.acceptanceMarker)))
		})
	}
}

// rotationAlreadyCompletedHistory is the minimal lifecycle-v1 history of a redelivered Begin that reports
// the rotation already completed.
func rotationAlreadyCompletedHistory(t *testing.T, acceptanceMarker bool) *historypb.History {
	t.Helper()
	dc := converter.GetDefaultDataConverter()
	payloads := func(v any) *commonpb.Payloads {
		p, err := dc.ToPayloads(v)
		require.NoError(t, err)
		return p
	}
	input := payloads(RotateEtcdEncryptionKeyInput{ClusterID: uuid.New()})
	now := timestamppb.Now()
	var events []*historypb.HistoryEvent
	add := func(eventType enumspb.EventType) *historypb.HistoryEvent {
		event := &historypb.HistoryEvent{EventId: int64(len(events) + 1), EventTime: now, EventType: eventType}
		events = append(events, event)
		return event
	}
	var firstTask int64
	addMarker := func(changeID string) {
		add(enumspb.EVENT_TYPE_MARKER_RECORDED).Attributes = &historypb.HistoryEvent_MarkerRecordedEventAttributes{
			MarkerRecordedEventAttributes: &historypb.MarkerRecordedEventAttributes{
				MarkerName: "Version",
				Details: map[string]*commonpb.Payloads{
					"change-id": payloads(changeID),
					"version":   payloads(1),
					// No search-attribute upsert event follows the marker, keeping event IDs minimal.
					"version-search-attribute-updated": payloads(false),
				},
				WorkflowTaskCompletedEventId: firstTask,
			},
		}
	}
	add(enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED).Attributes = &historypb.HistoryEvent_WorkflowExecutionStartedEventAttributes{
		WorkflowExecutionStartedEventAttributes: &historypb.WorkflowExecutionStartedEventAttributes{
			WorkflowType: &commonpb.WorkflowType{Name: "RotateEtcdEncryptionKey"},
			TaskQueue:    &taskqueuepb.TaskQueue{Name: "replay"},
			Input:        input,
		},
	}
	addWorkflowTask := func() int64 {
		scheduled := add(enumspb.EVENT_TYPE_WORKFLOW_TASK_SCHEDULED)
		scheduled.Attributes = &historypb.HistoryEvent_WorkflowTaskScheduledEventAttributes{
			WorkflowTaskScheduledEventAttributes: &historypb.WorkflowTaskScheduledEventAttributes{
				TaskQueue: &taskqueuepb.TaskQueue{Name: "replay"},
			},
		}
		started := add(enumspb.EVENT_TYPE_WORKFLOW_TASK_STARTED)
		started.Attributes = &historypb.HistoryEvent_WorkflowTaskStartedEventAttributes{
			WorkflowTaskStartedEventAttributes: &historypb.WorkflowTaskStartedEventAttributes{ScheduledEventId: scheduled.GetEventId()},
		}
		completed := add(enumspb.EVENT_TYPE_WORKFLOW_TASK_COMPLETED)
		completed.Attributes = &historypb.HistoryEvent_WorkflowTaskCompletedEventAttributes{
			WorkflowTaskCompletedEventAttributes: &historypb.WorkflowTaskCompletedEventAttributes{
				ScheduledEventId: scheduled.GetEventId(),
				StartedEventId:   started.GetEventId(),
			},
		}
		return completed.GetEventId()
	}
	firstTask = addWorkflowTask()
	addMarker(etcdKeyRotationLifecycleChangeID)
	if acceptanceMarker {
		addMarker(etcdKeyRotationBeginAcceptanceChangeID)
	}
	scheduled := add(enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED)
	// The SDK numbers activities by the event ID their schedule command takes.
	scheduled.Attributes = &historypb.HistoryEvent_ActivityTaskScheduledEventAttributes{
		ActivityTaskScheduledEventAttributes: &historypb.ActivityTaskScheduledEventAttributes{
			ActivityId:                   strconv.FormatInt(scheduled.GetEventId(), 10),
			ActivityType:                 &commonpb.ActivityType{Name: "BeginEtcdEncryptionKeyRotation"},
			TaskQueue:                    &taskqueuepb.TaskQueue{Name: "replay"},
			WorkflowTaskCompletedEventId: firstTask,
		},
	}
	started := add(enumspb.EVENT_TYPE_ACTIVITY_TASK_STARTED)
	started.Attributes = &historypb.HistoryEvent_ActivityTaskStartedEventAttributes{
		ActivityTaskStartedEventAttributes: &historypb.ActivityTaskStartedEventAttributes{ScheduledEventId: scheduled.GetEventId()},
	}
	add(enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED).Attributes = &historypb.HistoryEvent_ActivityTaskCompletedEventAttributes{
		ActivityTaskCompletedEventAttributes: &historypb.ActivityTaskCompletedEventAttributes{
			Result:           payloads(activities.BeginEtcdEncryptionKeyRotationResult{AlreadyCompleted: true}),
			ScheduledEventId: scheduled.GetEventId(),
			StartedEventId:   started.GetEventId(),
		},
	}
	lastTask := addWorkflowTask()
	add(enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_COMPLETED).Attributes = &historypb.HistoryEvent_WorkflowExecutionCompletedEventAttributes{
		WorkflowExecutionCompletedEventAttributes: &historypb.WorkflowExecutionCompletedEventAttributes{
			WorkflowTaskCompletedEventId: lastTask,
		},
	}
	return &historypb.History{Events: events}
}
