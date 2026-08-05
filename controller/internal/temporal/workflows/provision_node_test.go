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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

// labelNodeTestWorkflow is a thin wrapper so the unexported labelNode helper can
// be driven through the Temporal test environment.
func labelNodeTestWorkflow(ctx workflow.Context, nodeID uuid.UUID, addConfigProfileLabels bool) error {
	return labelNode(ctx, nodeID, addConfigProfileLabels)
}

// setupLabelNodeEnv builds a focused test env that registers the real DB
// activities labelNode needs and a capturing mock for LabelNode.
func setupLabelNodeEnv(t *testing.T, database db.DB, profilesDir string) (
	env *testsuite.TestWorkflowEnvironment,
	captured *activities.LabelNodeInput,
	called *bool,
) {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env = testSuite.NewTestWorkflowEnvironment()
	env.SetWorkerOptions(worker.Options{DeadlockDetectionTimeout: 5 * time.Second})

	a := activities.NewActivities(config.Config{ProfilesDir: profilesDir}, database, nil, nil, nil, "", nil)

	env.RegisterActivity(a.GetNode)
	env.RegisterActivity(a.GetClusterForNode)
	env.RegisterWorkflow(labelNodeTestWorkflow)

	captured = &activities.LabelNodeInput{}
	wasCalled := false
	called = &wasCalled
	env.OnActivity(a.LabelNode, mock.Anything, mock.Anything).
		Return(activities.LabelNodeResult{}, nil).
		Run(func(args mock.Arguments) {
			*called = true
			*captured = args.Get(1).(activities.LabelNodeInput)
		}).Maybe()

	return env, captured, called
}

func TestLabelNodeAddsMachineIDLabel(t *testing.T) {
	database, res := setupTestDB(t, seedOpts{forgeMachineID: "machine-abc123"})
	env, captured, called := setupLabelNodeEnv(t, database, res.profilesDir)

	env.ExecuteWorkflow(labelNodeTestWorkflow, res.nodeID, false)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.True(t, *called, "LabelNode should be called when ForgeMachineID is set")
	require.Contains(t, captured.Labels, constants.LabelKeyMachineID+"=machine-abc123")
}

func TestLabelNodeSkipsMachineIDLabelWhenEmpty(t *testing.T) {
	database, res := setupTestDB(t, seedOpts{forgeMachineID: ""})
	env, _, called := setupLabelNodeEnv(t, database, res.profilesDir)

	env.ExecuteWorkflow(labelNodeTestWorkflow, res.nodeID, false)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.False(t, *called, "LabelNode should not be called when there are no labels to apply")
}
