// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
	rolloutkubevip "github.com/dsx-ai-factory/nvke/controller/internal/rollout/guards/kubevip"
	rolloutetcd "github.com/dsx-ai-factory/nvke/controller/internal/rollout/operations/etcd"
	rollouttemporal "github.com/dsx-ai-factory/nvke/controller/internal/rollout/temporal"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
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

func TestRewriteEtcdEncryptedObjects_PollsAcknowledgedOperationID(t *testing.T) {
	nodeID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
	env, a := setupEnableEtcdEncryptionWorkflowEnv(t)
	// Model a recorded result preserved by reset. A fresh Start activity cannot
	// adopt a different persisted RUNNING operation.
	env.OnActivity(a.StartEtcdEncryptionRewrite, mock.Anything, mock.Anything).
		Return(activities.EtcdEncryptionRewriteOperation{
			OperationID: "acknowledged-before-reset",
			Phase:       activities.EtcdEncryptionOperationPhaseRunning,
		}, nil).Once()
	env.OnActivity(a.GetEtcdEncryptionRewriteStatus, mock.Anything,
		mock.MatchedBy(func(input activities.EtcdEncryptionRewriteInput) bool {
			return input.OperationID == "acknowledged-before-reset"
		})).Return(activities.EtcdEncryptionRewriteOperation{
		OperationID: "acknowledged-before-reset",
		Phase:       activities.EtcdEncryptionOperationPhaseSucceeded,
	}, nil).Once()

	env.ExecuteWorkflow(testRewriteEtcdEncryptedObjectsWorkflow, nodeID)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestRewriteEtcdEncryptedObjects_RecoversAcknowledgedOperationAfterAgentRestart(t *testing.T) {
	node := db.Node{ID: uuid.New(), Name: "cp-0", ControlPlane: true}
	env, _ := setupEnableEtcdEncryptionWorkflowEnv(t)
	store := &testutil.MockDB{}
	store.On("GetNode", mock.Anything, node.ID).Return(node, nil)

	// Model only the remote process loss. Execute the real Start and polling
	// activities inside the existing workflow, including its timer commands.
	var mu sync.Mutex
	var persistedID string
	var active, processLost bool
	starts := 0
	nodeAgent := &agenttest.FakeClient{
		GetEncryptedObjectsRewriteHook: func(_ db.Node, id string) (*pb.EtcdEncryptionAtRestObjectsRewriteOperation, error) {
			mu.Lock()
			defer mu.Unlock()
			if persistedID != id {
				return nil, status.Error(codes.NotFound, "not found")
			}
			state := pb.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_RUNNING
			if active && processLost {
				state = pb.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_SUCCEEDED
			}
			return &pb.EtcdEncryptionAtRestObjectsRewriteOperation{OperationId: persistedID, State: state}, nil
		},
		StartEncryptedObjectsRewriteHook: func(_ db.Node, id string) (*pb.EtcdEncryptionAtRestObjectsRewriteOperation, error) {
			mu.Lock()
			defer mu.Unlock()
			if persistedID != "" && persistedID != id {
				return nil, status.Error(codes.FailedPrecondition, "foreign operation")
			}
			persistedID, active = id, true
			starts++
			return &pb.EtcdEncryptionAtRestObjectsRewriteOperation{
				OperationId: id,
				State:       pb.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_RUNNING,
			}, nil
		},
	}
	a := activities.NewActivities(config.Config{}, store, nil, nodeAgent, "", nil, "")
	env.RegisterActivity(a.StartEtcdEncryptionRewrite)
	env.RegisterActivity(a.GetEtcdEncryptionRewriteStatus)
	env.SetOnActivityCompletedListener(func(info *activity.Info, _ converter.EncodedValue, err error) {
		if info.ActivityType.Name == "StartEtcdEncryptionRewrite" && err == nil {
			mu.Lock()
			active, processLost = false, true // acknowledged RUNNING remains durable
			mu.Unlock()
		}
	})

	env.ExecuteWorkflow(testRewriteEtcdEncryptedObjectsRecoveryWorkflow, node.ID)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.True(t, processLost)
	require.Equal(t, 2, starts, "polling must restart the acknowledged operation once")
	require.Len(t, nodeAgent.StartEncryptedObjectsRewriteIDs, 2)
	require.Equal(t, nodeAgent.StartEncryptedObjectsRewriteIDs[0], nodeAgent.StartEncryptedObjectsRewriteIDs[1])
	for _, id := range nodeAgent.GetEncryptedObjectsRewriteStatusIDs {
		require.Equal(t, persistedID, id)
	}
	store.AssertNumberOfCalls(t, "GetNode", 3)
	store.AssertExpectations(t)
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

func testRewriteEtcdEncryptedObjectsRecoveryWorkflow(ctx workflow.Context, nodeID uuid.UUID) error {
	return rewriteEtcdEncryptedObjectsWithTimeout(ctx, nodeID, 2*time.Minute)
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

func TestValidateEtcdEncryptionRewriteOperationFailedPhaseHasFixedRemedy(t *testing.T) {
	_, err := validateEtcdEncryptionRewriteOperation("op", activities.EtcdEncryptionRewriteOperation{
		OperationID: "op", Phase: activities.EtcdEncryptionOperationPhaseFailed, ErrorMessage: "upstream-body-marker",
	})
	require.EqualError(t, err, "encrypted-object rewrite failed; inspect node rewrite state")
}
