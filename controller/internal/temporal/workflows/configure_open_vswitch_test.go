// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

func configureOpenVSwitchTestWorkflow(ctx workflow.Context, nodeID uuid.UUID) error {
	return configureCNIPrerequisites(ctx, nodeID)
}

func TestConfigureCNIPrerequisitesUsesTypedOpenVSwitchActivityForOVN(t *testing.T) {
	nodeID := uuid.New()
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	var a *activities.Activities
	env.OnActivity(a.GetClusterForNode, mock.Anything, nodeID).
		Return(db.Cluster{CniPlugin: db.CNIOvnKubernetes}, nil).Once()
	env.OnActivity(
		a.ConfigureOpenVSwitch,
		mock.Anything,
		activities.ConfigureOpenVSwitchInput{NodeID: nodeID},
	).Return(activities.ConfigureOpenVSwitchResult{Path: "rpc"}, nil).Once()

	env.ExecuteWorkflow(configureOpenVSwitchTestWorkflow, nodeID)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestConfigureCNIPrerequisitesSkipsOpenVSwitchForOtherCNI(t *testing.T) {
	nodeID := uuid.New()
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	var a *activities.Activities
	env.OnActivity(a.GetClusterForNode, mock.Anything, nodeID).
		Return(db.Cluster{CniPlugin: db.CNICalico}, nil).Once()

	env.ExecuteWorkflow(configureOpenVSwitchTestWorkflow, nodeID)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}
