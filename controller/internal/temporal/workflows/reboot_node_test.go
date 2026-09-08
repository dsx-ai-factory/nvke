// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
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

func TestRebootCAPINode_SoftRebootSucceeds_NoHardFallback(t *testing.T) {
	nodeID := uuid.New()

	env, a := setupRebootNodeWorkflowEnv(t)
	env.OnActivity(a.UpdateNodeConditions, mock.Anything, mock.Anything).Return(nil).Once()
	env.OnActivity(a.SoftRebootNode, mock.Anything, mock.Anything).Return(activities.SoftRebootNodeResult{}, nil).Once()

	env.ExecuteWorkflow(testRebootCAPINodeWorkflow, nodeID)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
	env.AssertNotCalled(t, "ApplyCAPIMachineRebootAnnotation", mock.Anything, mock.Anything)
}

func TestRebootCAPINode_SoftRebootFails_FallsBackToHard(t *testing.T) {
	nodeID := uuid.New()

	env, a := setupRebootNodeWorkflowEnv(t)
	env.OnActivity(a.UpdateNodeConditions, mock.Anything, mock.Anything).Return(nil).Once()
	env.OnActivity(a.SoftRebootNode, mock.Anything, mock.Anything).Return(activities.SoftRebootNodeResult{}, errors.New("node-agent unreachable")).Once()
	env.OnActivity(a.ApplyCAPIMachineRebootAnnotation, mock.Anything, mock.Anything).Return(
		activities.ApplyCAPIMachineRebootAnnotationResult{RebootStartedTime: time.Now()}, nil,
	).Once()

	env.ExecuteWorkflow(testRebootCAPINodeWorkflow, nodeID)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestRebootCAPINode_SoftRebootCanceled_PropagatesWithoutFallback(t *testing.T) {
	nodeID := uuid.New()

	env, a := setupRebootNodeWorkflowEnv(t)
	env.OnActivity(a.UpdateNodeConditions, mock.Anything, mock.Anything).Return(nil).Once()
	env.OnActivity(a.SoftRebootNode, mock.Anything, mock.Anything).Return(
		activities.SoftRebootNodeResult{}, temporal.NewCanceledError(),
	).Once()

	env.ExecuteWorkflow(testRebootCAPINodeWorkflow, nodeID)

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	env.AssertExpectations(t)
	env.AssertNotCalled(t, "ApplyCAPIMachineRebootAnnotation", mock.Anything, mock.Anything)
}

// A confirmed soft reboot should never fall back to hard.
func TestWaitForNodeReboot_SoftRebootConfirmed(t *testing.T) {
	nodeID := uuid.New()

	env, a := setupRebootNodeWorkflowEnv(t)
	env.OnActivity(a.WaitForNodeReboot, mock.Anything, mock.Anything).Return(activities.WaitForNodeRebootResult{BootID: "new-boot-id"}, nil).Once()

	input := RebootNodeInput{
		NodeID:                   nodeID,
		InitialTimeout:           30 * time.Minute,
		SoftRebootConfirmTimeout: 5 * time.Minute,
		PollingInterval:          time.Minute,
	}
	env.ExecuteWorkflow(testWaitForNodeRebootWorkflow, input)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
	env.AssertNotCalled(t, "ApplyCAPIMachineRebootAnnotation", mock.Anything, mock.Anything)
}

// A soft reboot that acked but produced no detectable change (the CAPD case) must fall back to
// hard within the same attempt, not wait out the whole timeout.
func TestWaitForNodeReboot_SoftRebootNotConfirmed_FallsBackToHardWithinSameAttempt(t *testing.T) {
	nodeID := uuid.New()

	env, a := setupRebootNodeWorkflowEnv(t)

	// First (bounded) wait times out waiting for the soft reboot to take effect.
	env.OnActivity(a.WaitForNodeReboot, mock.Anything, mock.MatchedBy(func(input activities.WaitForNodeRebootInput) bool {
		return input.NodeID == nodeID
	})).Return(activities.WaitForNodeRebootResult{}, errors.New("deadline exceeded")).Once()

	env.OnActivity(a.ApplyCAPIMachineRebootAnnotation, mock.Anything, mock.Anything).Return(
		activities.ApplyCAPIMachineRebootAnnotationResult{RebootStartedTime: time.Now()}, nil,
	).Once()

	// Second wait, after the hard fallback, succeeds.
	env.OnActivity(a.WaitForNodeReboot, mock.Anything, mock.MatchedBy(func(input activities.WaitForNodeRebootInput) bool {
		return input.NodeID == nodeID
	})).Return(activities.WaitForNodeRebootResult{BootID: "new-boot-id"}, nil).Once()

	input := RebootNodeInput{
		NodeID:                   nodeID,
		InitialTimeout:           30 * time.Minute,
		SoftRebootConfirmTimeout: 5 * time.Minute,
		PollingInterval:          time.Minute,
	}
	env.ExecuteWorkflow(testWaitForNodeRebootWorkflow, input)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestWaitForNodeReboot_SoftRebootNotConfirmed_HardFallbackAlsoFails(t *testing.T) {
	nodeID := uuid.New()

	env, a := setupRebootNodeWorkflowEnv(t)
	env.OnActivity(a.WaitForNodeReboot, mock.Anything, mock.Anything).Return(activities.WaitForNodeRebootResult{}, errors.New("deadline exceeded")).Once()
	env.OnActivity(a.ApplyCAPIMachineRebootAnnotation, mock.Anything, mock.Anything).Return(
		activities.ApplyCAPIMachineRebootAnnotationResult{}, errors.New("machine not found"),
	).Once()

	input := RebootNodeInput{
		NodeID:                   nodeID,
		InitialTimeout:           30 * time.Minute,
		SoftRebootConfirmTimeout: 5 * time.Minute,
		PollingInterval:          time.Minute,
	}
	env.ExecuteWorkflow(testWaitForNodeRebootWorkflow, input)

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	require.Contains(t, env.GetWorkflowError().Error(), "hard reboot fallback failed")
	env.AssertExpectations(t)
}

func testRebootCAPINodeWorkflow(ctx workflow.Context, nodeID uuid.UUID) error {
	_, err := rebootCAPINode(ctx, nodeID, 1, "test")
	return err
}

func testWaitForNodeRebootWorkflow(ctx workflow.Context, input RebootNodeInput) error {
	return waitForNodeReboot(ctx, input, 1, "previous-boot-id", workflow.Now(ctx).UTC())
}

func setupRebootNodeWorkflowEnv(t *testing.T) (*testsuite.TestWorkflowEnvironment, *activities.Activities) {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	env.SetWorkerOptions(worker.Options{
		DeadlockDetectionTimeout: 5 * time.Second,
	})

	a := &activities.Activities{}
	env.RegisterActivity(a.UpdateNodeConditions)
	env.RegisterActivity(a.SoftRebootNode)
	env.RegisterActivity(a.ApplyCAPIMachineRebootAnnotation)
	env.RegisterActivity(a.WaitForNodeReboot)

	return env, a
}
