// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

func TestWaitForControlPlaneGracefulShutdown_UnsupportedRPC_Skips(t *testing.T) {
	nodeID := uuid.New()
	unsupportedErr := temporal.NewNonRetryableApplicationError(
		"agent on node cp-1 does not support control-plane graceful shutdown RPCs",
		activities.UnsupportedControlPlaneGracefulShutdownRPCErrorType,
		errors.New("rpc error: code = Unimplemented desc = unknown method"),
	)

	env, a := setupGracefulShutdownWorkflowEnv(t)
	env.OnActivity(a.StartControlPlaneGracefulShutdown, mock.Anything, mock.MatchedBy(func(input activities.StartControlPlaneGracefulShutdownInput) bool {
		return input.NodeID == nodeID && input.StopKubeAPIServer
	})).Return(activities.ControlPlaneGracefulShutdownOperation{}, unsupportedErr).Once()

	env.ExecuteWorkflow(testWaitForControlPlaneGracefulShutdownWorkflow, TeardownNodeInput{NodeID: nodeID})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestWaitForControlPlaneGracefulShutdown_StartSucceeded_Completes(t *testing.T) {
	nodeID := uuid.New()

	env, a := setupGracefulShutdownWorkflowEnv(t)
	env.OnActivity(a.StartControlPlaneGracefulShutdown, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input activities.StartControlPlaneGracefulShutdownInput) (activities.ControlPlaneGracefulShutdownOperation, error) {
			return activities.ControlPlaneGracefulShutdownOperation{
				ID:    input.OperationID,
				Phase: activities.ControlPlaneGracefulShutdownPhaseSucceeded,
			}, nil
		},
	).Once()

	env.ExecuteWorkflow(testWaitForControlPlaneGracefulShutdownWorkflow, TeardownNodeInput{NodeID: nodeID})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestWaitForControlPlaneGracefulShutdown_PollsUntilSucceeded(t *testing.T) {
	nodeID := uuid.New()

	env, a := setupGracefulShutdownWorkflowEnv(t)
	env.OnActivity(a.StartControlPlaneGracefulShutdown, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input activities.StartControlPlaneGracefulShutdownInput) (activities.ControlPlaneGracefulShutdownOperation, error) {
			return activities.ControlPlaneGracefulShutdownOperation{
				ID:    input.OperationID,
				Phase: activities.ControlPlaneGracefulShutdownPhaseRunning,
				Step:  "stop-kube-apiserver",
			}, nil
		},
	).Once()
	env.OnActivity(a.GetControlPlaneGracefulShutdownStatus, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input activities.GetControlPlaneGracefulShutdownStatusInput) (activities.ControlPlaneGracefulShutdownOperation, error) {
			return activities.ControlPlaneGracefulShutdownOperation{
				ID:    input.OperationID,
				Phase: activities.ControlPlaneGracefulShutdownPhaseSucceeded,
			}, nil
		},
	).Once()

	env.ExecuteWorkflow(testWaitForControlPlaneGracefulShutdownWorkflow, TeardownNodeInput{NodeID: nodeID})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestWaitForControlPlaneGracefulShutdown_Failed_ReturnsError(t *testing.T) {
	nodeID := uuid.New()

	env, a := setupGracefulShutdownWorkflowEnv(t)
	env.OnActivity(a.StartControlPlaneGracefulShutdown, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input activities.StartControlPlaneGracefulShutdownInput) (activities.ControlPlaneGracefulShutdownOperation, error) {
			return activities.ControlPlaneGracefulShutdownOperation{
				ID:    input.OperationID,
				Phase: activities.ControlPlaneGracefulShutdownPhaseFailed,
				Step:  "stop-kube-apiserver",
				Error: "apiserver did not exit",
			}, nil
		},
	).Once()

	env.ExecuteWorkflow(testWaitForControlPlaneGracefulShutdownWorkflow, TeardownNodeInput{NodeID: nodeID})

	require.True(t, env.IsWorkflowCompleted())
	err := env.GetWorkflowError()
	require.Error(t, err)
	require.Contains(t, err.Error(), "control-plane graceful shutdown failed at stop-kube-apiserver")
	require.Contains(t, err.Error(), "apiserver did not exit")
	env.AssertExpectations(t)
}

func testWaitForControlPlaneGracefulShutdownWorkflow(ctx workflow.Context, input TeardownNodeInput) error {
	return waitForControlPlaneGracefulShutdown(ctx, input)
}

func setupGracefulShutdownWorkflowEnv(t *testing.T) (*testsuite.TestWorkflowEnvironment, *activities.Activities) {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	env.SetWorkerOptions(worker.Options{
		DeadlockDetectionTimeout: 5 * time.Second,
	})

	a := &activities.Activities{}
	env.RegisterActivity(a.StartControlPlaneGracefulShutdown)
	env.RegisterActivity(a.GetControlPlaneGracefulShutdownStatus)

	return env, a
}

func TestValidateControlPlaneGracefulShutdownOperation(t *testing.T) {
	t.Parallel()

	operationID := "wf-id/node-id/k8s-control-plane-node-graceful-shutdown"

	tests := []struct {
		name      string
		operation activities.ControlPlaneGracefulShutdownOperation
		wantErr   string
	}{
		{
			name: "running",
			operation: activities.ControlPlaneGracefulShutdownOperation{
				ID:    operationID,
				Phase: activities.ControlPlaneGracefulShutdownPhaseRunning,
			},
		},
		{
			name: "succeeded",
			operation: activities.ControlPlaneGracefulShutdownOperation{
				ID:    operationID,
				Phase: activities.ControlPlaneGracefulShutdownPhaseSucceeded,
			},
		},
		{
			name: "failed",
			operation: activities.ControlPlaneGracefulShutdownOperation{
				ID:    operationID,
				Phase: activities.ControlPlaneGracefulShutdownPhaseFailed,
				Step:  "stop-kube-apiserver",
				Error: "apiserver did not exit",
			},
		},
		{
			name: "empty operation ID",
			operation: activities.ControlPlaneGracefulShutdownOperation{
				Phase: activities.ControlPlaneGracefulShutdownPhaseRunning,
			},
			wantErr: "empty operation ID",
		},
		{
			name: "mismatched operation ID",
			operation: activities.ControlPlaneGracefulShutdownOperation{
				ID:    "other-operation-id",
				Phase: activities.ControlPlaneGracefulShutdownPhaseRunning,
			},
			wantErr: "agent returned operation ID",
		},
		{
			name: "unspecified phase",
			operation: activities.ControlPlaneGracefulShutdownOperation{
				ID:    operationID,
				Phase: activities.ControlPlaneGracefulShutdownPhaseUnspecified,
			},
			wantErr: "unspecified operation phase",
		},
		{
			name: "empty phase",
			operation: activities.ControlPlaneGracefulShutdownOperation{
				ID: operationID,
			},
			wantErr: "unspecified operation phase",
		},
		{
			name: "unknown phase",
			operation: activities.ControlPlaneGracefulShutdownOperation{
				ID:    operationID,
				Phase: "bogus",
			},
			wantErr: "unknown operation phase",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateControlPlaneGracefulShutdownOperation(operationID, tt.operation)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
