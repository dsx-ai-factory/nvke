// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
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

func TestRebootCAPINode_SoftRebootSucceeds_DoesNotHardReboot(t *testing.T) {
	nodeID := uuid.New()

	env, a := setupRebootCAPINodeWorkflowEnv(t)
	env.OnActivity(a.UpdateNodeConditions, mock.Anything, mock.Anything).Return(nil).Once()
	env.OnActivity(a.SoftRebootNode, mock.Anything, mock.Anything).Return(activities.SoftRebootNodeResult{}, nil).Once()
	// No expectation is set for ApplyCAPIMachineRebootAnnotation: the mock framework fails the
	// test if it is invoked without a matching expectation, so this also asserts it is skipped.

	env.ExecuteWorkflow(testRebootCAPINodeWorkflow, nodeID, 1, "test reboot")

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var rebootStartedTime time.Time
	require.NoError(t, env.GetWorkflowResult(&rebootStartedTime))
	require.False(t, rebootStartedTime.IsZero())

	env.AssertExpectations(t)
}

func TestRebootCAPINode_SoftRebootFails_FallsBackToHardReboot(t *testing.T) {
	nodeID := uuid.New()
	hardRebootTime := time.Now().UTC()

	env, a := setupRebootCAPINodeWorkflowEnv(t)
	env.OnActivity(a.UpdateNodeConditions, mock.Anything, mock.Anything).Return(nil).Once()
	env.OnActivity(a.SoftRebootNode, mock.Anything, mock.Anything).
		Return(activities.SoftRebootNodeResult{}, temporal.NewApplicationError("soft reboot unreachable", "Unavailable")).Once()
	env.OnActivity(a.ApplyCAPIMachineRebootAnnotation, mock.Anything, mock.Anything).
		Return(activities.ApplyCAPIMachineRebootAnnotationResult{RebootStartedTime: hardRebootTime}, nil).Once()

	env.ExecuteWorkflow(testRebootCAPINodeWorkflow, nodeID, 1, "test reboot")

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var rebootStartedTime time.Time
	require.NoError(t, env.GetWorkflowResult(&rebootStartedTime))
	require.Equal(t, hardRebootTime, rebootStartedTime)

	env.AssertExpectations(t)
}

func TestRebootCAPINode_SoftRebootCanceled_DoesNotHardReboot(t *testing.T) {
	nodeID := uuid.New()

	env, a := setupRebootCAPINodeWorkflowEnv(t)
	env.OnActivity(a.UpdateNodeConditions, mock.Anything, mock.Anything).Return(nil).Once()
	env.OnActivity(a.SoftRebootNode, mock.Anything, mock.Anything).
		Return(activities.SoftRebootNodeResult{}, temporal.NewCanceledError("soft reboot canceled")).Once()
	// No expectation is set for ApplyCAPIMachineRebootAnnotation: a cancellation must propagate,
	// not fall back to a hard reboot.

	env.ExecuteWorkflow(testRebootCAPINodeWorkflow, nodeID, 1, "test reboot")

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())

	env.AssertExpectations(t)
}

func testRebootCAPINodeWorkflow(ctx workflow.Context, nodeID uuid.UUID, rebootAttempt int, reason string) (time.Time, error) {
	return rebootCAPINode(ctx, nodeID, rebootAttempt, reason)
}

func setupRebootCAPINodeWorkflowEnv(t *testing.T) (*testsuite.TestWorkflowEnvironment, *activities.Activities) {
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

	return env, a
}
