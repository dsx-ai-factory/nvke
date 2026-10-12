// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
	rolloutkubevip "github.com/dsx-ai-factory/nvke/controller/internal/rollout/guards/kubevip"
	rolloutoidc "github.com/dsx-ai-factory/nvke/controller/internal/rollout/operations/oidc"
	rollouttemporal "github.com/dsx-ai-factory/nvke/controller/internal/rollout/temporal"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

func TestEnableOIDCWorkflow_CompletesThenWritesBackAndMarks(t *testing.T) {
	clusterID := uuid.MustParse("650e8400-e29b-41d4-a716-446655440000")
	nodeID := uuid.MustParse("650e8400-e29b-41d4-a716-446655440001")

	env, a := setupEnableOIDCWorkflowEnv(t)
	expectOIDCClusterLoad(env, clusterID, nodeID)
	expectOIDCRolloutPhase(env, clusterID, rollout.OutcomeApplied)

	env.OnActivity(a.MergeOIDCKubeadmConfig, mock.Anything,
		activities.MergeOIDCKubeadmConfigInput{ClusterID: clusterID}).
		Return(activities.MergeOIDCKubeadmConfigResult{Changed: true}, nil).Once()
	// MarkOIDCEnabled completes enablement: it flips the marker and releases the
	// enablement lock in one atomic write, so there is no separate lock-release step.
	env.OnActivity(a.MarkOIDCEnabled, mock.Anything,
		activities.MarkOIDCEnabledInput{ClusterID: clusterID}).
		Return(activities.MarkOIDCEnabledResult{}, nil).Once()

	env.ExecuteWorkflow(EnableOIDC, EnableOIDCInput{ClusterID: clusterID})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestEnableOIDCWorkflow_HaltsBeforeWriteBackOnRolloutFailure(t *testing.T) {
	clusterID := uuid.MustParse("650e8400-e29b-41d4-a716-446655440000")
	nodeID := uuid.MustParse("650e8400-e29b-41d4-a716-446655440001")

	env, a := setupEnableOIDCWorkflowEnv(t)
	expectOIDCClusterLoad(env, clusterID, nodeID)
	expectOIDCRolloutPhase(env, clusterID, rollout.OutcomeFailedNotRestored)

	// The write-back and marker must NEVER run when the rollout does not complete,
	// so the marker cannot be flipped on a partial control plane.
	env.OnActivity(a.MergeOIDCKubeadmConfig, mock.Anything, mock.Anything).
		Return(activities.MergeOIDCKubeadmConfigResult{}, nil).Maybe()
	env.OnActivity(a.MarkOIDCEnabled, mock.Anything, mock.Anything).
		Return(activities.MarkOIDCEnabledResult{}, nil).Maybe()
	// On failure the enablement lock MUST be released (with a failure message) so
	// UpdateCluster is not wedged at 409.
	env.OnActivity(a.ClearOIDCUpdateInProgress, mock.Anything,
		activities.ClearOIDCUpdateInProgressInput{ClusterID: clusterID, FailureMessage: oidcFailureMessage}).
		Return(activities.ClearOIDCUpdateInProgressResult{}, nil).Once()

	env.ExecuteWorkflow(EnableOIDC, EnableOIDCInput{ClusterID: clusterID})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	env.AssertNotCalled(t, "MergeOIDCKubeadmConfig", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "MarkOIDCEnabled", mock.Anything, mock.Anything)
	env.AssertCalled(t, "ClearOIDCUpdateInProgress", mock.Anything,
		activities.ClearOIDCUpdateInProgressInput{ClusterID: clusterID, FailureMessage: oidcFailureMessage})
}

func TestOIDCRolloutSpecMatchesSWERollout(t *testing.T) {
	withoutGuard := oidcRolloutSpec(3, etcdRolloutPhaseOptions{})
	require.Equal(t, rollout.StrategySerial, withoutGuard.Strategy)
	require.Equal(t, rollout.RollbackHaltOnAnyFailure, withoutGuard.RollbackPolicy)
	require.Equal(t, 3, withoutGuard.MinHealthyReplicas.MinHealthy)
	require.Len(t, withoutGuard.NodePreflights, 1)
	require.Empty(t, withoutGuard.NodeGuards)

	withGuard := oidcRolloutSpec(3, etcdRolloutPhaseOptions{withKubeVipNodeGuard: true})
	require.Len(t, withGuard.NodeGuards, 1)
	require.Equal(t, rolloutkubevip.GuardNameKubeVipDrainRestore, withGuard.NodeGuards[0].Name)
}

func expectOIDCClusterLoad(env *testsuite.TestWorkflowEnvironment, clusterID, nodeID uuid.UUID) {
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "cp-0", IP: "10.0.0.10", ControlPlane: true}
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
}

func expectOIDCRolloutPhase(env *testsuite.TestWorkflowEnvironment, clusterID uuid.UUID, outcome rollout.Outcome) {
	operationName := rolloutoidc.OperationName
	env.OnActivity(rollouttemporal.ActivityValidate, mock.Anything,
		mock.MatchedBy(func(req *rollouttemporal.ValidateRequest) bool {
			return req.OperationName == operationName && req.ClusterID == clusterID
		})).Return(nil).Once()
	env.OnActivity(rollouttemporal.ActivityClusterPreflight, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(rollouttemporal.ActivityBuildApplyToNodePayload, mock.Anything,
		mock.MatchedBy(func(req *rollouttemporal.BuildApplyToNodePayloadRequest) bool {
			return req.OperationName == operationName && req.ClusterID == clusterID
		})).Return(&rollouttemporal.BuildApplyToNodePayloadResponse{
		Payload: rollout.ApplyToNodePayload([]byte(`{"files":[{"path":"/etc/kubernetes/oidc/auth-config.yaml","content":"eA==","mode":384,"owner":"root:root"}],"authConfigPath":"/etc/kubernetes/oidc/auth-config.yaml"}`)),
	}, nil).Once()
	env.OnActivity(rollouttemporal.ActivityNodePreflight, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(rollouttemporal.ActivityNodeGuardPreApply, mock.Anything,
		mock.MatchedBy(func(req *rollouttemporal.NodeGuardPreApplyRequest) bool {
			return req.GuardName == rolloutkubevip.GuardNameKubeVipDrainRestore && req.ClusterID == clusterID
		})).Return(&rollouttemporal.NodeGuardPreApplyResponse{
		State: rollout.StatelessGuardState(rolloutkubevip.GuardNameKubeVipDrainRestore),
	}, nil).Maybe()
	env.OnActivity(rollouttemporal.ActivityApplyToNode, mock.Anything,
		mock.MatchedBy(func(req *rollouttemporal.ApplyToNodeRequest) bool {
			return req.OperationName == operationName && req.ClusterID == clusterID
		})).Return(func() *rollouttemporal.ApplyToNodeResponse {
		resp := &rollouttemporal.ApplyToNodeResponse{Outcome: outcome}
		if outcome != rollout.OutcomeApplied && outcome != rollout.OutcomeUnchanged {
			resp.ApplyErr = "injected apply failure"
		}
		return resp
	}(), nil).Once()
	env.OnActivity(rollouttemporal.ActivityNodeGuardPostApply, mock.Anything, mock.Anything).Return(nil).Maybe()
	env.OnActivity(rollouttemporal.ActivityRollbackNode, mock.Anything, mock.Anything).Return(nil).Maybe()
}

func setupEnableOIDCWorkflowEnv(t *testing.T) (*testsuite.TestWorkflowEnvironment, *activities.Activities) {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	env.SetWorkerOptions(worker.Options{DeadlockDetectionTimeout: 5 * time.Second})
	rollouttemporal.NewActivities(&noOpRolloutLoader{}, rollout.NewRegistry()).Register(env)
	rollouttemporal.NewClusterLoaderActivity(&noOpClusterDataLoader{}, "50051").Register(env)

	a := &activities.Activities{}
	return env, a
}
