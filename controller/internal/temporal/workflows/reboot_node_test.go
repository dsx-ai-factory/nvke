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

	env.ExecuteWorkflow(testRebootCAPINodeWorkflow, nodeID, true)

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

	env.ExecuteWorkflow(testRebootCAPINodeWorkflow, nodeID, true)

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

	env.ExecuteWorkflow(testRebootCAPINodeWorkflow, nodeID, true)

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	env.AssertExpectations(t)
	env.AssertNotCalled(t, "ApplyCAPIMachineRebootAnnotation", mock.Anything, mock.Anything)
}

// SoftRebootEnabled=false (CAPD, and the NICo containers lane) skips the soft RPC entirely.
func TestRebootCAPINode_SoftRebootDisabled_GoesStraightToHard(t *testing.T) {
	nodeID := uuid.New()

	env, a := setupRebootNodeWorkflowEnv(t)
	env.OnActivity(a.UpdateNodeConditions, mock.Anything, mock.Anything).Return(nil).Once()
	env.OnActivity(a.ApplyCAPIMachineRebootAnnotation, mock.Anything, mock.Anything).Return(
		activities.ApplyCAPIMachineRebootAnnotationResult{RebootStartedTime: time.Now()}, nil,
	).Once()

	env.ExecuteWorkflow(testRebootCAPINodeWorkflow, nodeID, false)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
	env.AssertNotCalled(t, "SoftRebootNode", mock.Anything, mock.Anything)
}

func TestWaitForNodeReboot_BootChangeDetected(t *testing.T) {
	nodeID := uuid.New()

	env, a := setupRebootNodeWorkflowEnv(t)
	env.OnActivity(a.WaitForNodeReboot, mock.Anything, mock.Anything).Return(activities.WaitForNodeRebootResult{BootID: "new-boot-id"}, nil).Once()

	input := RebootNodeInput{
		NodeID:          nodeID,
		InitialTimeout:  30 * time.Minute,
		MaxTimeout:      time.Hour,
		PollingInterval: time.Minute,
	}
	env.ExecuteWorkflow(testWaitForNodeRebootWorkflow, input)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func testRebootCAPINodeWorkflow(ctx workflow.Context, nodeID uuid.UUID, softRebootEnabled bool) error {
	_, err := rebootCAPINode(ctx, RebootNodeInput{NodeID: nodeID, SoftRebootEnabled: softRebootEnabled}, 1)
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
