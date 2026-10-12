// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
	rolloutetcd "github.com/dsx-ai-factory/nvke/controller/internal/rollout/operations/etcd"
	rollouttemporal "github.com/dsx-ai-factory/nvke/controller/internal/rollout/temporal"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

func TestRotateEtcdEncryptionKeyWorkflow_CompletesThroughUnguardedRolloutExecutor(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	nodeID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "cp-0", IP: "10.0.0.10", ControlPlane: true}

	env, a := setupEnableEtcdEncryptionWorkflowEnv(t)
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
