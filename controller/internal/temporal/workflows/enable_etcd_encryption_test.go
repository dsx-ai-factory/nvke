// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package workflows

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/rollout"
	rolloutkubevip "github.com/NVIDIA/nke/controller/internal/rollout/guards/kubevip"
	rolloutetcd "github.com/NVIDIA/nke/controller/internal/rollout/operations/etcd"
	rollouttemporal "github.com/NVIDIA/nke/controller/internal/rollout/temporal"
	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

func TestEnableEtcdEncryptionWorkflow_CompletesThroughRolloutExecutor(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	nodeID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")

	env, a := setupEnableEtcdEncryptionWorkflowEnv(t)
	expectEtcdEncryptionPreparation(t, env, a, clusterID, nodeID)
	expectEtcdRolloutPhase(t, env, clusterID, rolloutetcd.OperationNameInitializeKubernetesAPIEncryption, true, rollout.OutcomeApplied)
	expectEtcdRolloutPhase(t, env, clusterID, rolloutetcd.OperationNamePromoteKubernetesAPIEncryptionProviders, false, rollout.OutcomeApplied)
	expectEtcdRolloutPhase(t, env, clusterID, rolloutetcd.OperationNameRemovePlainTextSecondaryEncryptionEntry, false, rollout.OutcomeApplied)

	env.OnActivity(a.StartEtcdEncryptionRewrite, mock.Anything, mock.Anything).
		Return(func(_ context.Context, input activities.EtcdEncryptionRewriteInput) (activities.EtcdEncryptionRewriteOperation, error) {
			return activities.EtcdEncryptionRewriteOperation{
				OperationID: input.OperationID,
				Phase:       activities.EtcdEncryptionOperationPhaseSucceeded,
			}, nil
		}).Once()
	env.OnActivity(
		a.MergeEtcdEncryptionKubeadmConfig,
		mock.Anything,
		activities.KubeadmConfigClusterInput{ClusterID: clusterID},
	).Return(activities.MergeEtcdEncryptionKubeadmConfigResult{Changed: true}, nil).Once()
	env.OnActivity(a.FinalizePendingEtcdEncryptionKey, mock.Anything, activities.EtcdEncryptionClusterInput{ClusterID: clusterID}).
		Return(nil).Once()
	env.OnActivity(a.CompleteEtcdEncryption, mock.Anything, activities.CompleteEtcdEncryptionInput{ClusterID: clusterID}).
		Return(activities.CompleteEtcdEncryptionResult{}, nil).Once()

	env.ExecuteWorkflow(EnableEtcdEncryption, EnableEtcdEncryptionInput{ClusterID: clusterID})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestEnableEtcdEncryptionWorkflow_ClearsProgressAfterExecutorFailure(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	nodeID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")

	env, a := setupEnableEtcdEncryptionWorkflowEnv(t)
	expectEtcdEncryptionPreparation(t, env, a, clusterID, nodeID)
	expectEtcdRolloutPhase(t, env, clusterID, rolloutetcd.OperationNameInitializeKubernetesAPIEncryption, true, rollout.OutcomeFailedNotRestored)
	env.OnActivity(
		a.ClearEtcdEncryptionUpdateInProgress,
		mock.Anything,
		mock.MatchedBy(func(input activities.ClearEtcdEncryptionUpdateInProgressInput) bool {
			return input.ClusterID == clusterID && input.FailureMessage == etcdEncryptionFailureMessage
		}),
	).Return(activities.ClearEtcdEncryptionUpdateInProgressResult{}, nil).Once()

	env.ExecuteWorkflow(EnableEtcdEncryption, EnableEtcdEncryptionInput{ClusterID: clusterID})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestRewriteEtcdEncryptedObjects_ResumesRunningOperation(t *testing.T) {
	nodeID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
	env, a := setupEnableEtcdEncryptionWorkflowEnv(t)
	env.OnActivity(a.StartEtcdEncryptionRewrite, mock.Anything, mock.Anything).
		Return(activities.EtcdEncryptionRewriteOperation{
			OperationID: "rewrite-from-prior-workflow-run",
			Phase:       activities.EtcdEncryptionOperationPhaseRunning,
		}, nil).Once()
	env.OnActivity(a.GetEtcdEncryptionRewriteStatus, mock.Anything,
		mock.MatchedBy(func(input activities.EtcdEncryptionRewriteInput) bool {
			return input.OperationID == "rewrite-from-prior-workflow-run"
		})).Return(activities.EtcdEncryptionRewriteOperation{
		OperationID: "rewrite-from-prior-workflow-run",
		Phase:       activities.EtcdEncryptionOperationPhaseSucceeded,
	}, nil).Once()

	env.ExecuteWorkflow(testRewriteEtcdEncryptedObjectsWorkflow, nodeID)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestRewriteEtcdEncryptedObjects_TimesOut(t *testing.T) {
	nodeID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
	env, a := setupEnableEtcdEncryptionWorkflowEnv(t)
	env.OnActivity(a.StartEtcdEncryptionRewrite, mock.Anything, mock.Anything).
		Return(func(_ context.Context, input activities.EtcdEncryptionRewriteInput) (activities.EtcdEncryptionRewriteOperation, error) {
			return activities.EtcdEncryptionRewriteOperation{
				OperationID: input.OperationID,
				Phase:       activities.EtcdEncryptionOperationPhaseRunning,
			}, nil
		}).Once()
	env.OnActivity(a.GetEtcdEncryptionRewriteStatus, mock.Anything, mock.Anything).
		Return(func(_ context.Context, input activities.EtcdEncryptionRewriteInput) (activities.EtcdEncryptionRewriteOperation, error) {
			return activities.EtcdEncryptionRewriteOperation{
				OperationID: input.OperationID,
				Phase:       activities.EtcdEncryptionOperationPhaseRunning,
			}, nil
		}).Once()

	env.ExecuteWorkflow(testRewriteEtcdEncryptedObjectsTimeoutWorkflow, nodeID)

	require.True(t, env.IsWorkflowCompleted())
	require.ErrorContains(t, env.GetWorkflowError(), "did not complete within 45s")
	env.AssertExpectations(t)
}

func TestEtcdEncryptionRolloutSpecMatchesSWERollout(t *testing.T) {
	withoutGuard := etcdEncryptionRolloutSpec(3, etcdRolloutPhaseOptions{})
	assert.Equal(t, rollout.StrategySerial, withoutGuard.Strategy)
	assert.Equal(t, rollout.RollbackCascadeOnUnrecoverable, withoutGuard.RollbackPolicy)
	assert.Equal(t, 3, withoutGuard.MinHealthyReplicas.MinHealthy)
	assert.Len(t, withoutGuard.ClusterPreflights, 2)
	assert.Len(t, withoutGuard.NodePreflights, 2)
	assert.Empty(t, withoutGuard.NodeGuards)

	withGuard := etcdEncryptionRolloutSpec(3, etcdRolloutPhaseOptions{withKubeVipNodeGuard: true})
	require.Len(t, withGuard.NodeGuards, 1)
	assert.Equal(t, rolloutkubevip.GuardNameKubeVipDrainRestore, withGuard.NodeGuards[0].Name)
}

func expectEtcdEncryptionPreparation(
	t *testing.T,
	env *testsuite.TestWorkflowEnvironment,
	a *activities.Activities,
	clusterID uuid.UUID,
	nodeID uuid.UUID,
) {
	t.Helper()
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "cp-0", IP: "10.0.0.10", ControlPlane: true}
	env.OnActivity(a.PrepareEtcdEncryption, mock.Anything, activities.PrepareEtcdEncryptionInput{ClusterID: clusterID}).
		Return(activities.PrepareEtcdEncryptionResult{NodeIDs: []uuid.UUID{nodeID}, ExecutorNodeID: nodeID}, nil).Once()
	env.OnActivity(a.ValidateEtcdEncryptionHealth, mock.Anything,
		activities.ValidateEtcdEncryptionHealthInput{ClusterID: clusterID, ExpectedNodeIDs: []uuid.UUID{nodeID}}).
		Return(nil).Once()
	env.OnActivity(
		a.ValidateEtcdEncryptionKubeadmConfig,
		mock.Anything,
		activities.KubeadmConfigClusterInput{ClusterID: clusterID},
	).Return(nil).Once()
	env.OnActivity(rollouttemporal.ActivityLoadClusterForRollout, mock.Anything,
		&rollouttemporal.LoadClusterForRolloutRequest{ClusterID: clusterID}).
		Return(&rollouttemporal.LoadClusterForRolloutResponse{
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
	env.OnActivity(a.EnsurePendingEtcdEncryptionKey, mock.Anything, activities.EtcdEncryptionClusterInput{ClusterID: clusterID}).
		Return(nil).Once()
}

func expectEtcdRolloutPhase(
	t *testing.T,
	env *testsuite.TestWorkflowEnvironment,
	clusterID uuid.UUID,
	operationName string,
	withKubeVipGuard bool,
	outcome rollout.Outcome,
) *testsuite.MockCallWrapper {
	t.Helper()
	env.OnActivity(rollouttemporal.ActivityValidate, mock.Anything,
		mock.MatchedBy(func(req *rollouttemporal.ValidateRequest) bool {
			return req.OperationName == operationName && req.ClusterID == clusterID
		})).Return(nil).Once()
	env.OnActivity(rollouttemporal.ActivityClusterPreflight, mock.Anything, mock.Anything).Return(nil).Times(2)
	env.OnActivity(rollouttemporal.ActivityBuildApplyToNodePayload, mock.Anything,
		mock.MatchedBy(func(req *rollouttemporal.BuildApplyToNodePayloadRequest) bool {
			return req.OperationName == operationName && req.ClusterID == clusterID
		})).Return(&rollouttemporal.BuildApplyToNodePayloadResponse{
		Payload: rollout.ApplyToNodePayload([]byte(`{"clusterId":"` + clusterID.String() + `"}`)),
	}, nil).Once()
	env.OnActivity(rollouttemporal.ActivityNodePreflight, mock.Anything, mock.Anything).Return(nil).Times(2)
	if withKubeVipGuard {
		env.OnActivity(rollouttemporal.ActivityNodeGuardPreApply, mock.Anything,
			mock.MatchedBy(func(req *rollouttemporal.NodeGuardPreApplyRequest) bool {
				return req.GuardName == rolloutkubevip.GuardNameKubeVipDrainRestore && req.ClusterID == clusterID
			})).Return(&rollouttemporal.NodeGuardPreApplyResponse{
			State: rollout.StatelessGuardState(rolloutkubevip.GuardNameKubeVipDrainRestore),
		}, nil).Once()
	}
	lastCall := env.OnActivity(rollouttemporal.ActivityApplyToNode, mock.Anything,
		mock.MatchedBy(func(req *rollouttemporal.ApplyToNodeRequest) bool {
			return req.OperationName == operationName && req.ClusterID == clusterID
		})).Return(func() *rollouttemporal.ApplyToNodeResponse {
		resp := &rollouttemporal.ApplyToNodeResponse{Outcome: outcome}
		if outcome != rollout.OutcomeApplied && outcome != rollout.OutcomeUnchanged {
			resp.ApplyErr = "injected apply failure"
		}
		return resp
	}(), nil).Once()
	if withKubeVipGuard {
		lastCall = env.OnActivity(rollouttemporal.ActivityNodeGuardPostApply, mock.Anything,
			mock.MatchedBy(func(req *rollouttemporal.NodeGuardPostApplyRequest) bool {
				return req.GuardName == rolloutkubevip.GuardNameKubeVipDrainRestore &&
					req.ClusterID == clusterID && req.Outcome == outcome
			})).Return(nil).Once()
	}
	return lastCall
}

func testRewriteEtcdEncryptedObjectsWorkflow(ctx workflow.Context, nodeID uuid.UUID) error {
	return rewriteEtcdEncryptedObjects(ctx, nodeID)
}

func testRewriteEtcdEncryptedObjectsTimeoutWorkflow(ctx workflow.Context, nodeID uuid.UUID) error {
	return rewriteEtcdEncryptedObjectsWithTimeout(ctx, nodeID, 45*time.Second)
}

type noOpRolloutLoader struct{}

func (*noOpRolloutLoader) LoadCluster(context.Context, uuid.UUID) (rollout.Cluster, error) {
	panic("mocked activities must short-circuit before LoadCluster")
}

type noOpClusterDataLoader struct{}

func (*noOpClusterDataLoader) LoadClusterData(context.Context, uuid.UUID) (db.Cluster, []db.Node, []db.Node, error) {
	return db.Cluster{}, nil, nil, nil
}

func setupEnableEtcdEncryptionWorkflowEnv(t *testing.T) (*testsuite.TestWorkflowEnvironment, *activities.Activities) {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	env.SetWorkerOptions(worker.Options{DeadlockDetectionTimeout: 5 * time.Second})
	rollouttemporal.NewActivities(&noOpRolloutLoader{}, rollout.NewRegistry()).Register(env)
	rollouttemporal.NewClusterLoaderActivity(&noOpClusterDataLoader{}, "50051").Register(env)

	a := &activities.Activities{}
	return env, a
}
