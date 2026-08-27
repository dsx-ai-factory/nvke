// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

const (
	beforeRemoveNodeRunbook = "evict-cnpg-node"
	afterRemoveNodeRunbook  = "delete-longhorn-node"
)

func TestTeardownNodeHooksSkippedWhenClusterNeverProvisioned(t *testing.T) {
	database, res := setupTestDB(t, seedOpts{
		clusterProvisioned: false,
		cpNodeJoined:       false,
	})

	env, _ := setupWorkflowEnv(t, database, res.profilesDir, workflowEnvOpts{})

	// ExecuteClusterRunbook is NOT mocked — if the workflow calls it,
	// the test will panic with an unexpected activity call.

	env.ExecuteWorkflow(TeardownNode, TeardownNodeInput{
		NodeID:      res.nodeID,
		AllowCPNode: true,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
}

func TestTeardownNodeHooksRunWhenClusterProvisioned(t *testing.T) {
	database, res := setupTestDB(t, seedOpts{
		clusterProvisioned: true,
		cpNodeJoined:       true,
	})

	env, a := setupWorkflowEnv(t, database, res.profilesDir, workflowEnvOpts{})

	// Expect beforeRemoveNode hook
	env.OnActivity(a.ExecuteClusterRunbook, mock.Anything, mock.MatchedBy(func(input activities.ExecuteClusterRunbookInput) bool {
		return input.RunbookName == beforeRemoveNodeRunbook
	})).Return(activities.ExecuteClusterRunbookResult{}, nil).Once()

	// Expect afterRemoveNode hook
	env.OnActivity(a.ExecuteClusterRunbook, mock.Anything, mock.MatchedBy(func(input activities.ExecuteClusterRunbookInput) bool {
		return input.RunbookName == afterRemoveNodeRunbook
	})).Return(activities.ExecuteClusterRunbookResult{}, nil).Once()

	var teardownSteps []string
	env.SetOnActivityStartedListener(func(activityInfo *activity.Info, _ context.Context, args converter.EncodedValues) {
		if activityInfo.ActivityType.Name != "ExecuteClusterRunbook" {
			teardownSteps = append(teardownSteps, activityInfo.ActivityType.Name)
			return
		}

		var input activities.ExecuteClusterRunbookInput
		require.NoError(t, args.Get(&input))
		teardownSteps = append(teardownSteps, input.RunbookName)
	})

	env.ExecuteWorkflow(TeardownNode, TeardownNodeInput{
		NodeID:      res.nodeID,
		AllowCPNode: true,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)

	beforeHookIndex := slices.Index(teardownSteps, beforeRemoveNodeRunbook)
	infraDeletionIndex := slices.Index(teardownSteps, "DeleteForgeInstance")
	kubeNodeDeletionIndex := slices.Index(teardownSteps, "DeleteKubeNode")
	afterHookIndex := slices.Index(teardownSteps, afterRemoveNodeRunbook)
	require.NotEqual(t, -1, beforeHookIndex)
	require.NotEqual(t, -1, infraDeletionIndex)
	require.NotEqual(t, -1, kubeNodeDeletionIndex)
	require.NotEqual(t, -1, afterHookIndex)
	require.Less(t, beforeHookIndex, infraDeletionIndex,
		"CNPG eviction must complete before infrastructure deletion")
	require.Less(t, infraDeletionIndex, kubeNodeDeletionIndex,
		"infrastructure deletion must complete before Kubernetes node deletion")
	require.Less(t, kubeNodeDeletionIndex, afterHookIndex,
		"Longhorn cleanup must run after Kubernetes node deletion")
}

func TestTeardownNodeHooksRunWhenNotProvisionedButCPJoined(t *testing.T) {
	database, res := setupTestDB(t, seedOpts{
		clusterProvisioned: false,
		cpNodeJoined:       true,
	})

	env, a := setupWorkflowEnv(t, database, res.profilesDir, workflowEnvOpts{})

	// Expect beforeRemoveNode hook
	env.OnActivity(a.ExecuteClusterRunbook, mock.Anything, mock.MatchedBy(func(input activities.ExecuteClusterRunbookInput) bool {
		return input.RunbookName == beforeRemoveNodeRunbook
	})).Return(activities.ExecuteClusterRunbookResult{}, nil).Once()

	// Expect afterRemoveNode hook
	env.OnActivity(a.ExecuteClusterRunbook, mock.Anything, mock.MatchedBy(func(input activities.ExecuteClusterRunbookInput) bool {
		return input.RunbookName == afterRemoveNodeRunbook
	})).Return(activities.ExecuteClusterRunbookResult{}, nil).Once()

	env.ExecuteWorkflow(TeardownNode, TeardownNodeInput{
		NodeID:      res.nodeID,
		AllowCPNode: true,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestTeardownNodeHooksSkippedWhenConfigProfileOmitted(t *testing.T) {
	database, res := setupTestDB(t, seedOpts{
		clusterProvisioned: true,
		cpNodeJoined:       true,
		omitConfigProfile:  true,
	})

	env, _ := setupWorkflowEnv(t, database, res.profilesDir, workflowEnvOpts{})

	// ExecuteClusterRunbook is NOT mocked — if the workflow calls it,
	// the test will panic with an unexpected activity call.
	env.ExecuteWorkflow(TeardownNode, TeardownNodeInput{
		NodeID:      res.nodeID,
		AllowCPNode: true,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
}

// Teardown hooks execute as cluster runbooks, so they must never be dispatched
// to the node being removed — on a force-delete that node is exactly the
// unreachable one.
func TestTeardownNodeHooksExcludeTargetNodeAsExecutor(t *testing.T) {
	database, res := setupTestDB(t, seedOpts{
		clusterProvisioned: true,
		cpNodeJoined:       true,
	})

	env, a := setupWorkflowEnv(t, database, res.profilesDir, workflowEnvOpts{})

	var hookInputs []activities.ExecuteClusterRunbookInput
	env.OnActivity(a.ExecuteClusterRunbook, mock.Anything, mock.MatchedBy(func(input activities.ExecuteClusterRunbookInput) bool {
		return input.RunbookName == beforeRemoveNodeRunbook || input.RunbookName == afterRemoveNodeRunbook
	})).Return(func(_ context.Context, input activities.ExecuteClusterRunbookInput) (activities.ExecuteClusterRunbookResult, error) {
		hookInputs = append(hookInputs, input)
		return activities.ExecuteClusterRunbookResult{}, nil
	}, nil).Twice()

	env.ExecuteWorkflow(TeardownNode, TeardownNodeInput{
		NodeID:      res.nodeID,
		AllowCPNode: true,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, hookInputs, 2)
	for _, input := range hookInputs {
		require.NotNil(t, input.ExcludeNodeID, "hook %s must exclude the target node", input.RunbookName)
		require.Equal(t, res.nodeID, *input.ExcludeNodeID)
		require.NotContains(t, input.RunbookEnv, "FORCE_UNREACHABLE",
			"hook %s must not advertise force mode on a normal teardown", input.RunbookName)
	}
}

// With no surviving joined CP there is no executor for the hooks —
// the only candidate would be the node under teardown. Same skip as the
// live-removal path when there is no surviving joined CP.
func TestTeardownNodeHooksSkippedWhenNoSurvivingJoinedCP(t *testing.T) {
	database, res := setupTestDB(t, seedOpts{
		clusterProvisioned:        true,
		onlyJoinedCPUnderTeardown: true,
	})

	env, _ := setupWorkflowEnv(t, database, res.profilesDir, workflowEnvOpts{
		omitClusterRunbookMock: true,
	})

	// ExecuteClusterRunbook is NOT mocked — if the workflow calls it,
	// the test fails with an unexpected activity call.
	env.ExecuteWorkflow(TeardownNode, TeardownNodeInput{
		NodeID:      res.nodeID,
		AllowCPNode: true,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
}

// A failing teardown hook must stall a normal teardown (the hooks protect
// data) but must not stall a force-delete, where the hook's waits can never
// be satisfied because the node is gone.
func TestTeardownNodeHookFailureToleratedOnlyWhenForceUnreachable(t *testing.T) {
	tests := []struct {
		name             string
		forceUnreachable bool
		wantErr          bool
	}{
		{name: "hook failure fails teardown", forceUnreachable: false, wantErr: true},
		{name: "hook failure tolerated when force unreachable", forceUnreachable: true, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, res := setupTestDB(t, seedOpts{
				clusterProvisioned: true,
				cpNodeJoined:       true,
			})

			env, a := setupWorkflowEnv(t, database, res.profilesDir, workflowEnvOpts{})

			var hookEnvs []map[string]string
			env.OnActivity(a.ExecuteClusterRunbook, mock.Anything, mock.MatchedBy(func(input activities.ExecuteClusterRunbookInput) bool {
				return input.RunbookName == beforeRemoveNodeRunbook || input.RunbookName == afterRemoveNodeRunbook
			})).Return(func(_ context.Context, input activities.ExecuteClusterRunbookInput) (activities.ExecuteClusterRunbookResult, error) {
				hookEnvs = append(hookEnvs, input.RunbookEnv)
				return activities.ExecuteClusterRunbookResult{}, errors.New("PVCs still exist: node is unreachable")
			}, nil).Maybe()

			if !tt.forceUnreachable {
				// The normal path retries the hook forever on purpose, so the
				// workflow never finishes on its own. Bound the test explicitly
				// rather than leaning on the harness' own execution timeout:
				// cancel after an hour of skipped time and assert the teardown
				// was still stuck in the hook.
				env.RegisterDelayedCallback(env.CancelWorkflow, time.Hour)
			}

			env.ExecuteWorkflow(TeardownNode, TeardownNodeInput{
				NodeID:           res.nodeID,
				AllowCPNode:      true,
				ForceUnreachable: tt.forceUnreachable,
			})

			require.True(t, env.IsWorkflowCompleted())
			if tt.wantErr {
				require.Error(t, env.GetWorkflowError())
				// Retries are unbounded without force_unreachable: the hook was
				// still being attempted when the cancel landed, well past the
				// force-delete budget.
				require.Greater(t, len(hookEnvs), forceUnreachableHookMaxAttempts,
					"the normal path must keep retrying a failing hook instead of giving up")
				for _, hookEnv := range hookEnvs {
					require.NotContains(t, hookEnv, "FORCE_UNREACHABLE",
						"hooks must not be told the node is unreachable on a normal teardown")
				}

				return
			}

			require.NoError(t, env.GetWorkflowError())
			// Both hooks ran and gave up: the retry budget must be bounded, or
			// the tolerate branch is unreachable and the workflow spins forever.
			require.Len(t, hookEnvs, 2*forceUnreachableHookMaxAttempts,
				"each hook should be attempted exactly forceUnreachableHookMaxAttempts times")
			for _, hookEnv := range hookEnvs {
				require.Equal(t, "true", hookEnv["FORCE_UNREACHABLE"],
					"hooks must be told the node is unreachable so they can skip dead-node waits")
			}
		})
	}
}

func TestTeardownNodeDrainAndRunbookActivitiesHaveHeartbeatTimeout(t *testing.T) {
	database, res := setupTestDB(t, seedOpts{
		clusterProvisioned: true,
		cpNodeJoined:       true,
		omitConfigProfile:  true,
	})

	env, _ := setupWorkflowEnv(t, database, res.profilesDir, workflowEnvOpts{})

	var observedRunbookHeartbeatTimeout time.Duration
	var observedDrainHeartbeatTimeout time.Duration
	var drainInput activities.DrainKubeNodeInput
	env.SetOnActivityStartedListener(func(activityInfo *activity.Info, _ context.Context, args converter.EncodedValues) {
		if activityInfo.ActivityType.Name == "ExecuteClusterRunbook" {
			observedRunbookHeartbeatTimeout = activityInfo.HeartbeatTimeout
		}
		if activityInfo.ActivityType.Name == "DrainKubeNode" {
			observedDrainHeartbeatTimeout = activityInfo.HeartbeatTimeout
			require.NoError(t, args.Get(&drainInput))
		}
	})

	env.ExecuteWorkflow(TeardownNode, TeardownNodeInput{
		NodeID:      res.nodeID,
		AllowCPNode: true,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, defaultRunbookActivityHeartbeatTimeout(), observedRunbookHeartbeatTimeout)
	require.Equal(t, time.Minute, observedDrainHeartbeatTimeout)
	require.False(t, drainInput.ForceDrainOnFailure, "control-plane drains must not force-delete pods by default")
}

func TestTeardownNodeCPNodeGuardSkipsTeardown(t *testing.T) {
	database, res := setupTestDB(t, seedOpts{
		clusterProvisioned: true,
		cpNodeJoined:       true,
		omitConfigProfile:  true,
	})

	env, _ := setupWorkflowEnv(t, database, res.profilesDir, workflowEnvOpts{
		omitClusterRunbookMock: true,
	})

	env.ExecuteWorkflow(TeardownNode, TeardownNodeInput{
		NodeID:      res.nodeID,
		AllowCPNode: false,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	_, err := database.GetNode(context.Background(), res.nodeID)
	require.NoError(t, err, "CP guard should skip teardown before deleting the node record")
}

func TestTeardownNodeSkipRemoveFromClusterResetsOnly(t *testing.T) {
	database, res := setupTestDB(t, seedOpts{
		clusterProvisioned: true,
		cpNodeJoined:       true,
		omitConfigProfile:  true,
	})

	env, a := setupWorkflowEnv(t, database, res.profilesDir, workflowEnvOpts{
		omitClusterRunbookMock:      true,
		omitKubeadmResetMock:        true,
		omitDeleteForgeInstanceMock: true,
	})
	env.OnActivity(a.KubeadmReset, mock.Anything, mock.MatchedBy(func(input activities.KubeadmResetInput) bool {
		return input.NodeID == res.nodeID &&
			len(input.SkipPhases) == 1 &&
			input.SkipPhases[0] == removeEtcdMember
	})).Return(activities.KubeadmResetResult{}, nil).Once()
	env.OnActivity(a.DeleteForgeInstance, mock.Anything, mock.Anything).Return(activities.DeleteForgeInstanceResult{}, nil).Once()

	env.ExecuteWorkflow(TeardownNode, TeardownNodeInput{
		NodeID:                res.nodeID,
		AllowCPNode:           true,
		SkipRemoveFromCluster: true,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)

	_, err := database.GetNode(context.Background(), res.nodeID)
	require.ErrorIs(t, err, db.ErrNotFound, "node record should be deleted after infrastructure teardown")
}

func TestTeardownNodeResetsOnlyWhenClusterIsDeletingDespiteInput(t *testing.T) {
	database, res := setupTestDB(t, seedOpts{
		clusterProvisioned: true,
		cpNodeJoined:       true,
		clusterDeleting:    true,
		workerNodePool:     true,
		omitConfigProfile:  true,
	})

	env, a := setupWorkflowEnv(t, database, res.profilesDir, workflowEnvOpts{
		omitClusterRunbookMock:      true,
		omitKubeadmResetMock:        true,
		omitDeleteForgeInstanceMock: true,
	})
	env.OnActivity(a.KubeadmReset, mock.Anything, mock.MatchedBy(func(input activities.KubeadmResetInput) bool {
		return input.NodeID == res.nodeID && len(input.SkipPhases) == 0
	})).Return(activities.KubeadmResetResult{}, nil).Once()
	env.OnActivity(a.DeleteForgeInstance, mock.Anything, mock.Anything).Return(activities.DeleteForgeInstanceResult{}, nil).Once()

	var activityNames []string
	env.SetOnActivityStartedListener(func(activityInfo *activity.Info, _ context.Context, _ converter.EncodedValues) {
		activityNames = append(activityNames, activityInfo.ActivityType.Name)
	})

	// SkipRemoveFromCluster left false on purpose: this is the input the outbox
	// failed to deliver.
	env.ExecuteWorkflow(TeardownNode, TeardownNodeInput{
		NodeID: res.nodeID,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
	require.NotContains(t, activityNames, "DrainKubeNode",
		"must not drain into a cluster that is being deleted")
	require.NotContains(t, activityNames, "DeleteKubeNode")

	_, err := database.GetNode(context.Background(), res.nodeID)
	require.ErrorIs(t, err, db.ErrNotFound, "node record should be deleted after infrastructure teardown")
}

// Tearing down the last joined CP with SkipRemoveFromCluster=false
// (e.g. DELETE /api/nodePools racing cluster delete) must not attempt drain —
// ExecuteClusterRunbook excludes the target, leaving zero executor candidates.
func TestTeardownNodeSkipsLiveRemovalWhenNoSurvivingJoinedCP(t *testing.T) {
	database, res := setupTestDB(t, seedOpts{
		clusterProvisioned:        true,
		onlyJoinedCPUnderTeardown: true,
		omitConfigProfile:         true,
	})

	env, a := setupWorkflowEnv(t, database, res.profilesDir, workflowEnvOpts{
		omitClusterRunbookMock:      true,
		omitKubeadmResetMock:        true,
		omitDeleteForgeInstanceMock: true,
	})
	env.OnActivity(a.KubeadmReset, mock.Anything, mock.MatchedBy(func(input activities.KubeadmResetInput) bool {
		return input.NodeID == res.nodeID &&
			len(input.SkipPhases) == 1 &&
			input.SkipPhases[0] == removeEtcdMember
	})).Return(activities.KubeadmResetResult{}, nil).Once()
	env.OnActivity(a.DeleteForgeInstance, mock.Anything, mock.Anything).Return(activities.DeleteForgeInstanceResult{}, nil).Once()

	var activityNames []string
	var runbookNames []string
	env.SetOnActivityStartedListener(func(activityInfo *activity.Info, _ context.Context, args converter.EncodedValues) {
		activityNames = append(activityNames, activityInfo.ActivityType.Name)
		if activityInfo.ActivityType.Name != "ExecuteClusterRunbook" {
			return
		}
		var input activities.ExecuteClusterRunbookInput
		if err := args.Get(&input); err != nil {
			return
		}
		runbookNames = append(runbookNames, input.RunbookName)
	})

	env.ExecuteWorkflow(TeardownNode, TeardownNodeInput{
		NodeID:      res.nodeID,
		AllowCPNode: true,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
	require.Empty(t, runbookNames, "etcd removal must not run without a surviving joined CP")
	require.NotContains(t, activityNames, "DeleteKubeNode")

	_, err := database.GetNode(context.Background(), res.nodeID)
	require.ErrorIs(t, err, db.ErrNotFound, "node record should be deleted after infrastructure teardown")
}

// When the only joined CP peer is itself being torn down, it cannot be relied
// on to serve the tenant API or execute runbooks. A check that only reads
// Joined treats the doomed peer as a survivor, drains through it, and fails
// terminally once its instance is gone.
func TestTeardownNodeSkipsLiveRemovalWhenOnlySurvivingCPIsDeleting(t *testing.T) {
	database, res := setupTestDB(t, seedOpts{
		clusterProvisioned:  true,
		cpNodeJoined:        true,
		survivingCPDeleting: true,
		workerNodePool:      true,
		omitConfigProfile:   true,
	})

	env, a := setupWorkflowEnv(t, database, res.profilesDir, workflowEnvOpts{
		omitClusterRunbookMock:      true,
		omitKubeadmResetMock:        true,
		omitDeleteForgeInstanceMock: true,
	})
	env.OnActivity(a.KubeadmReset, mock.Anything, mock.MatchedBy(func(input activities.KubeadmResetInput) bool {
		return input.NodeID == res.nodeID && len(input.SkipPhases) == 0
	})).Return(activities.KubeadmResetResult{}, nil).Once()
	env.OnActivity(a.DeleteForgeInstance, mock.Anything, mock.Anything).Return(activities.DeleteForgeInstanceResult{}, nil).Once()

	var activityNames []string
	env.SetOnActivityStartedListener(func(activityInfo *activity.Info, _ context.Context, _ converter.EncodedValues) {
		activityNames = append(activityNames, activityInfo.ActivityType.Name)
	})

	env.ExecuteWorkflow(TeardownNode, TeardownNodeInput{
		NodeID: res.nodeID,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
	require.NotContains(t, activityNames, "DrainKubeNode",
		"must not drain through a control plane that is itself being torn down")
	require.NotContains(t, activityNames, "DeleteKubeNode")

	_, err := database.GetNode(context.Background(), res.nodeID)
	require.ErrorIs(t, err, db.ErrNotFound, "node record should be deleted after infrastructure teardown")
}

func TestTeardownNodeEtcdDryRunMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		exitCode         int
		allowQuorumLoss  bool
		allowSingleVoter bool
		wantErr          string
	}{
		{
			name:     "safe exit code",
			exitCode: etcdDryRunExitCodeSafe,
		},
		{
			name:     "quorum loss blocked",
			exitCode: etcdDryRunExitCodeQuorum,
			wantErr:  "etcd member removal would break quorum",
		},
		{
			name:            "quorum loss allowed",
			exitCode:        etcdDryRunExitCodeQuorum,
			allowQuorumLoss: true,
		},
		{
			name:     "single voter blocked",
			exitCode: etcdDryRunExitCodeVoter,
			wantErr:  "single-voter",
		},
		{
			name:             "single voter allowed",
			exitCode:         etcdDryRunExitCodeVoter,
			allowSingleVoter: true,
		},
		{
			name:     "unexpected exit code",
			exitCode: 99,
			wantErr:  "unexpected exit code",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			exitCode := tt.exitCode
			database, res := setupTestDB(t, seedOpts{
				clusterProvisioned: true,
				cpNodeJoined:       true,
				omitConfigProfile:  true,
			})

			env, _ := setupWorkflowEnv(t, database, res.profilesDir, workflowEnvOpts{
				etcdDryRunExitCode: &exitCode,
			})

			env.ExecuteWorkflow(TeardownNode, TeardownNodeInput{
				NodeID:           res.nodeID,
				AllowCPNode:      true,
				AllowQuorumLoss:  tt.allowQuorumLoss,
				AllowSingleVoter: tt.allowSingleVoter,
			})

			require.True(t, env.IsWorkflowCompleted())
			err := env.GetWorkflowError()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestTeardownNodeForceUnreachableContinuesAfterDrainFailure(t *testing.T) {
	database, res := setupTestDB(t, seedOpts{
		clusterProvisioned: true,
		cpNodeJoined:       true,
		workerNodePool:     true,
		omitConfigProfile:  true,
	})

	env, _ := setupWorkflowEnv(t, database, res.profilesDir, workflowEnvOpts{
		failDrainActivity: true,
	})

	env.ExecuteWorkflow(TeardownNode, TeardownNodeInput{
		NodeID:           res.nodeID,
		AllowCPNode:      true,
		ForceUnreachable: true,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
}

func TestTeardownNodeForceUnreachableContinuesAfterGracefulShutdownFailure(t *testing.T) {
	database, res := setupTestDB(t, seedOpts{
		clusterProvisioned: true,
		cpNodeJoined:       true,
		omitConfigProfile:  true,
	})

	env, _ := setupWorkflowEnv(t, database, res.profilesDir, workflowEnvOpts{
		failGracefulShutdownStart: true,
	})

	env.ExecuteWorkflow(TeardownNode, TeardownNodeInput{
		NodeID:           res.nodeID,
		AllowCPNode:      true,
		ForceUnreachable: true,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
}

func TestTeardownNodeWorkerPoolSkipsEtcdRemoval(t *testing.T) {
	database, res := setupTestDB(t, seedOpts{
		clusterProvisioned: true,
		cpNodeJoined:       true,
		workerNodePool:     true,
		omitConfigProfile:  true,
	})

	env, _ := setupWorkflowEnv(t, database, res.profilesDir, workflowEnvOpts{})

	var activityNames []string
	var runbookNames []string
	var drainInput activities.DrainKubeNodeInput
	env.SetOnActivityStartedListener(func(activityInfo *activity.Info, _ context.Context, args converter.EncodedValues) {
		activityNames = append(activityNames, activityInfo.ActivityType.Name)
		if activityInfo.ActivityType.Name == "DrainKubeNode" {
			require.NoError(t, args.Get(&drainInput))
		}
		if activityInfo.ActivityType.Name != "ExecuteClusterRunbook" {
			return
		}

		var input activities.ExecuteClusterRunbookInput
		if err := args.Get(&input); err != nil {
			return
		}
		runbookNames = append(runbookNames, input.RunbookName)
	})

	env.ExecuteWorkflow(TeardownNode, TeardownNodeInput{
		NodeID:      res.nodeID,
		AllowCPNode: true,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.NotContains(t, runbookNames, "drain-kube-node")
	require.NotContains(t, runbookNames, "delete-kube-node")
	require.NotContains(t, runbookNames, runbookEtcdMemberRemove)
	require.Contains(t, activityNames, "DrainKubeNode")
	require.Contains(t, activityNames, "DeleteKubeNode")
	require.True(t, drainInput.ForceDrainOnFailure, "worker drains must preserve force-delete fallback")
}

func TestTeardownNodeDeletesInfrastructureBeforeKubeNode(t *testing.T) {
	tests := []struct {
		name                  string
		infraMode             activities.InfraMode
		infraDeletionActivity string
	}{
		{
			name:                  "forge",
			infraMode:             activities.InfraModeForge,
			infraDeletionActivity: "DeleteForgeInstance",
		},
		{
			name:                  "capi",
			infraMode:             activities.InfraModeCAPI,
			infraDeletionActivity: "DeleteCAPIMachineForNode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, res := setupTestDB(t, seedOpts{
				clusterProvisioned: true,
				cpNodeJoined:       true,
				omitConfigProfile:  true,
			})

			env, _ := setupWorkflowEnv(t, database, res.profilesDir, workflowEnvOpts{
				infraMode: tt.infraMode,
				kubeadmResetErr: temporal.NewNonRetryableApplicationError(
					"node-agent unavailable",
					"NodeAgentUnavailable",
					nil,
				),
				deleteKubeNodeErr: errors.New("tenant API unavailable"),
			})

			var activityNames []string
			env.SetOnActivityStartedListener(func(activityInfo *activity.Info, _ context.Context, _ converter.EncodedValues) {
				activityNames = append(activityNames, activityInfo.ActivityType.Name)
			})

			env.ExecuteWorkflow(TeardownNode, TeardownNodeInput{
				NodeID:      res.nodeID,
				AllowCPNode: true,
			})

			require.True(t, env.IsWorkflowCompleted())
			require.ErrorContains(t, env.GetWorkflowError(), "tenant API unavailable")

			infraDeletionIndex := slices.Index(activityNames, tt.infraDeletionActivity)
			kubeadmResetIndex := slices.Index(activityNames, "KubeadmReset")
			kubeNodeDeletionIndex := slices.Index(activityNames, "DeleteKubeNode")
			require.NotEqual(t, -1, infraDeletionIndex, "infrastructure deletion activity should run")
			require.NotEqual(t, -1, kubeadmResetIndex, "kubeadm reset activity should run")
			require.NotEqual(t, -1, kubeNodeDeletionIndex, "Kubernetes node deletion activity should run")
			require.Less(t, kubeadmResetIndex, infraDeletionIndex,
				"best-effort kubeadm reset must run before infrastructure deletion")
			require.Less(t, infraDeletionIndex, kubeNodeDeletionIndex,
				"infrastructure deletion must complete before Kubernetes node deletion starts")

			_, err := database.GetNode(context.Background(), res.nodeID)
			require.NoError(t, err, "failed Kubernetes cleanup should retain the node row for retry and diagnosis")
		})
	}
}

func TestTeardownNodeDoesNotDeleteKubeNodeWhenInfrastructureDeletionFails(t *testing.T) {
	tests := []struct {
		name                  string
		infraMode             activities.InfraMode
		infraDeletionActivity string
	}{
		{
			name:                  "forge",
			infraMode:             activities.InfraModeForge,
			infraDeletionActivity: "DeleteForgeInstance",
		},
		{
			name:                  "capi",
			infraMode:             activities.InfraModeCAPI,
			infraDeletionActivity: "DeleteCAPIMachineForNode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, res := setupTestDB(t, seedOpts{
				clusterProvisioned: true,
				cpNodeJoined:       true,
				omitConfigProfile:  true,
			})

			env, _ := setupWorkflowEnv(t, database, res.profilesDir, workflowEnvOpts{
				infraMode: tt.infraMode,
				infraDeletionErr: temporal.NewNonRetryableApplicationError(
					"provider deletion failed",
					"ProviderDeletionFailed",
					nil,
				),
			})

			var activityNames []string
			env.SetOnActivityStartedListener(func(activityInfo *activity.Info, _ context.Context, _ converter.EncodedValues) {
				activityNames = append(activityNames, activityInfo.ActivityType.Name)
			})

			env.ExecuteWorkflow(TeardownNode, TeardownNodeInput{
				NodeID:      res.nodeID,
				AllowCPNode: true,
			})

			require.True(t, env.IsWorkflowCompleted())
			require.ErrorContains(t, env.GetWorkflowError(), "provider deletion failed")
			require.Contains(t, activityNames, tt.infraDeletionActivity)
			require.NotContains(t, activityNames, "DeleteKubeNode",
				"Kubernetes node deletion must wait for infrastructure deletion to complete")

			_, err := database.GetNode(context.Background(), res.nodeID)
			require.NoError(t, err, "failed infrastructure deletion should retain the node row for retry and diagnosis")
		})
	}
}

// --- helpers ---

var testHooksConfig = fmt.Sprintf(`
hooks:
  beforeRemoveNode:
    - runbook:
        name: %s
  afterRemoveNode:
    - runbook:
        name: %s
`, beforeRemoveNodeRunbook, afterRemoveNodeRunbook)

type seedOpts struct {
	clusterProvisioned        bool
	cpNodeJoined              bool
	onlyJoinedCPUnderTeardown bool // primary node is the sole joined CP; no surviving peer
	survivingCPDeleting       bool // the peer CP is joined but is itself being torn down
	clusterDeleting           bool // cluster is being torn down, whatever the workflow input says
	workerNodePool            bool
	configProfileName         string
	omitConfigProfile         bool
	forgeMachineID            string
}

type seedResult struct {
	nodeID      uuid.UUID
	profilesDir string
}

func setupTestDB(t *testing.T, opts seedOpts) (db.DB, seedResult) {
	t.Helper()
	ctx := context.Background()

	bunDB := testutil.SetupDB(ctx, t,
		(*db.Site)(nil),
		(*db.Cluster)(nil),
		(*db.ControlPlaneConfiguration)(nil),
		(*db.NodePool)(nil),
		(*db.NodeSet)(nil),
		(*db.Node)(nil),
	)
	database := db.NewDB(bunDB)

	siteID := uuid.New()
	clusterID := uuid.New()
	nodePoolID := uuid.New()
	nodeSetID := uuid.New()

	configProfileName := opts.configProfileName
	if configProfileName == "" && !opts.omitConfigProfile {
		configProfileName = "test-profile"
	}
	profilesDir := t.TempDir()
	if configProfileName != "" {
		writeConfigProfile(t, profilesDir, configProfileName, testHooksConfig)
	}

	_, err := database.RegisterSite(ctx, &db.Site{
		ID:        siteID,
		VIPRanges: []string{},
	})
	require.NoError(t, err)

	clusterConditions := db.ClusterConditions{}
	if opts.clusterProvisioned {
		clusterConditions.Provisioned = db.NewCondition(true, "Provisioned cluster")
	}
	if opts.clusterDeleting {
		clusterConditions.Deleting = db.NewCondition(true, "Cluster deletion in progress")
	}
	_, err = database.CreateCluster(ctx, &db.Cluster{
		ID:                clusterID,
		Name:              "test-cluster",
		OrgID:             "test-org",
		SiteID:            siteID,
		VpcID:             uuid.New(),
		KubernetesVersion: "1.30.0",
		Conditions:        clusterConditions,
	})
	require.NoError(t, err)

	testNodePoolID := nodePoolID
	testNodeSetID := nodeSetID
	if opts.workerNodePool {
		cpNodePoolID := nodePoolID
		cpNodeSetID := nodeSetID

		testNodePoolID = uuid.New()
		testNodeSetID = uuid.New()
		_, err = database.CreateNodePool(ctx, &db.NodePool{
			ID:             testNodePoolID,
			ClusterID:      clusterID,
			Name:           "worker-pool",
			NodeNamePrefix: "worker",
			ControlPlane:   false,
			OSProfile:      "test",
			ConfigProfile:  configProfileName,
			SubnetIDs:      []uuid.UUID{},
			VpcPrefixIDs:   []uuid.UUID{},
			SSHKeyGroupIDs: []uuid.UUID{},
			NodeLabels:     []string{},
		})
		require.NoError(t, err)

		_, err = database.CreateNodeSet(ctx, &db.NodeSet{
			ID:                     testNodeSetID,
			NodePoolID:             testNodePoolID,
			OSProfile:              "test",
			ConfigProfile:          configProfileName,
			CapiBootstrapFiles:     []db.CloudInitFile{},
			CapiPreKubeadmCommands: []string{},
			SubnetIDs:              []uuid.UUID{},
			VpcPrefixIDs:           []uuid.UUID{},
		})
		require.NoError(t, err)

		nodePoolID = cpNodePoolID
		nodeSetID = cpNodeSetID
	}

	_, err = database.CreateNodePool(ctx, &db.NodePool{
		ID:             nodePoolID,
		ClusterID:      clusterID,
		Name:           "cp-pool",
		NodeNamePrefix: "cp",
		ControlPlane:   true,
		OSProfile:      "test",
		ConfigProfile:  configProfileName,
		SubnetIDs:      []uuid.UUID{},
		VpcPrefixIDs:   []uuid.UUID{},
		SSHKeyGroupIDs: []uuid.UUID{},
		NodeLabels:     []string{},
	})
	require.NoError(t, err)

	_, err = database.CreateNodeSet(ctx, &db.NodeSet{
		ID:                     nodeSetID,
		NodePoolID:             nodePoolID,
		OSProfile:              "test",
		ConfigProfile:          configProfileName,
		CapiBootstrapFiles:     []db.CloudInitFile{},
		CapiPreKubeadmCommands: []string{},
		SubnetIDs:              []uuid.UUID{},
		VpcPrefixIDs:           []uuid.UUID{},
	})
	require.NoError(t, err)

	nodeID := uuid.New()
	nodeConditions := db.NodeConditions{}
	if opts.onlyJoinedCPUnderTeardown {
		nodeConditions.Joined = db.NewCondition(true, "Joined")
	}
	_, err = database.CreateNode(ctx, &db.Node{
		ID:             nodeID,
		ClusterID:      clusterID,
		NodePoolID:     testNodePoolID,
		NodeSetID:      testNodeSetID,
		Name:           "test-node",
		IP:             "10.0.0.1",
		ForgeMachineID: opts.forgeMachineID,
		Conditions:     nodeConditions,
	})
	require.NoError(t, err)

	// A separate surviving joined CP keeps the tenant API reachable and serves
	// as the runbook executor when the node under teardown is removed.
	if opts.cpNodeJoined && !opts.onlyJoinedCPUnderTeardown {
		peerConditions := db.NodeConditions{
			Joined: db.NewCondition(true, "Joined"),
		}
		if opts.survivingCPDeleting {
			peerConditions.Deleting = db.NewCondition(true, "Node deletion in progress")
		}
		_, err = database.CreateNode(ctx, &db.Node{
			ID:         uuid.New(),
			ClusterID:  clusterID,
			NodePoolID: nodePoolID,
			NodeSetID:  nodeSetID,
			Name:       "cp-node",
			IP:         "10.0.0.2",
			Conditions: peerConditions,
		})
		require.NoError(t, err)
	}

	return database, seedResult{nodeID: nodeID, profilesDir: profilesDir}
}

type workflowEnvOpts struct {
	etcdDryRunExitCode          *int
	failDrainActivity           bool
	failGracefulShutdownStart   bool
	deleteKubeNodeErr           error
	kubeadmResetErr             error
	infraMode                   activities.InfraMode
	infraDeletionErr            error
	omitClusterRunbookMock      bool
	omitKubeadmResetMock        bool
	omitDeleteForgeInstanceMock bool
}

func setupWorkflowEnv(t *testing.T, database db.DB, profilesDir string, opts workflowEnvOpts) (*testsuite.TestWorkflowEnvironment, *activities.Activities) {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	env.SetWorkerOptions(worker.Options{
		DeadlockDetectionTimeout: 5 * time.Second,
	})

	a := activities.NewActivities(config.Config{ProfilesDir: profilesDir}, database, nil, nil, nil, "", nil)

	// Register real DB activities
	env.RegisterActivity(a.GetNodePoolForNode)
	env.RegisterActivity(a.GetNode)
	env.RegisterActivity(a.GetNodeProvisioningContext)
	env.RegisterActivity(a.GetClusterForNode)
	env.RegisterActivity(a.ListControlPlaneNodes)
	env.RegisterActivity(a.GetNodeConfigProfile)
	env.RegisterActivity(a.DeleteNode)

	// Mock external activities and workflow operations
	env.OnActivity(a.LabelNode, mock.Anything, mock.Anything).Return(activities.LabelNodeResult{}, nil).Maybe()
	drainErr := error(nil)
	if opts.failDrainActivity {
		drainErr = errors.New("node unreachable")
	}
	env.OnActivity(a.DrainKubeNode, mock.Anything, mock.Anything).Return(drainErr).Maybe()
	env.OnActivity(a.DeleteKubeNode, mock.Anything, mock.Anything).Return(opts.deleteKubeNodeErr).Maybe()
	if !opts.omitClusterRunbookMock {
		env.OnActivity(a.ExecuteClusterRunbook, mock.Anything, mock.MatchedBy(func(input activities.ExecuteClusterRunbookInput) bool {
			return input.RunbookName != beforeRemoveNodeRunbook && input.RunbookName != afterRemoveNodeRunbook
		})).Return(
			clusterRunbookResultFunc(opts),
			nil,
		).Maybe()
	}
	env.OnActivity(a.StartControlPlaneGracefulShutdown, mock.Anything, mock.Anything).Return(
		gracefulShutdownStartResultFunc(opts),
		nil,
	).Maybe()
	if !opts.omitKubeadmResetMock {
		env.OnActivity(a.KubeadmReset, mock.Anything, mock.Anything).
			Return(activities.KubeadmResetResult{}, opts.kubeadmResetErr).Maybe()
	}
	if opts.infraMode == activities.InfraModeCAPI {
		env.OnActivity(a.DeleteCAPIMachineForNode, mock.Anything, mock.Anything).Return(opts.infraDeletionErr).Maybe()
	} else if !opts.omitDeleteForgeInstanceMock {
		env.OnActivity(a.DeleteForgeInstance, mock.Anything, mock.Anything).
			Return(activities.DeleteForgeInstanceResult{}, opts.infraDeletionErr).Maybe()
	}
	infraMode := opts.infraMode
	if infraMode == "" {
		infraMode = activities.InfraModeForge
	}
	env.OnActivity(a.GetNodeProvisioningContext, mock.Anything, mock.Anything).Return(activities.NodeProvisioningContext{
		InfraMode: infraMode,
	}, nil).Maybe()
	env.OnActivity(a.UpdateNodeConditions, mock.Anything, mock.Anything).Return(nil).Maybe()
	env.OnRequestCancelExternalWorkflow(mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	return env, a
}

func clusterRunbookResultFunc(opts workflowEnvOpts) func(context.Context, activities.ExecuteClusterRunbookInput) (activities.ExecuteClusterRunbookResult, error) {
	return func(_ context.Context, input activities.ExecuteClusterRunbookInput) (activities.ExecuteClusterRunbookResult, error) {
		if input.RunbookName == runbookEtcdMemberRemove && len(input.RunbookArgs) >= 2 && input.RunbookArgs[0] == "--dry-run" {
			exitCode := etcdDryRunExitCodeSafe
			if opts.etcdDryRunExitCode != nil {
				exitCode = *opts.etcdDryRunExitCode
			}
			return activities.ExecuteClusterRunbookResult{
				ExitCode: exitCode,
				Output:   fmt.Sprintf("etcd dry-run exit code %d", exitCode),
			}, nil
		}

		return activities.ExecuteClusterRunbookResult{
			NodeID:   uuid.New(),
			NodeName: "executor-node",
			NodeIP:   "10.0.0.2",
		}, nil
	}
}

func gracefulShutdownStartResultFunc(opts workflowEnvOpts) func(context.Context, activities.StartControlPlaneGracefulShutdownInput) (activities.ControlPlaneGracefulShutdownOperation, error) {
	return func(_ context.Context, input activities.StartControlPlaneGracefulShutdownInput) (activities.ControlPlaneGracefulShutdownOperation, error) {
		if opts.failGracefulShutdownStart {
			return activities.ControlPlaneGracefulShutdownOperation{
				ID:    input.OperationID,
				Phase: activities.ControlPlaneGracefulShutdownPhaseFailed,
				Step:  "stop-kube-apiserver",
				Error: "apiserver stuck",
			}, nil
		}

		return activities.ControlPlaneGracefulShutdownOperation{
			ID:    input.OperationID,
			Phase: activities.ControlPlaneGracefulShutdownPhaseSucceeded,
		}, nil
	}
}

func writeConfigProfile(t *testing.T, profilesDir, name, content string) {
	t.Helper()
	dir := filepath.Join(profilesDir, "configProfiles")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(content), 0o644))
}
