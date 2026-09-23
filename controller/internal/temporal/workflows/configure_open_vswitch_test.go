// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/temporal/activities"
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
		mock.MatchedBy(func(input activities.ConfigureOpenVSwitchInput) bool {
			return input.NodeID == nodeID &&
				strings.HasPrefix(input.OperationID, "open-vswitch/") &&
				strings.HasSuffix(input.OperationID, "/"+nodeID.String())
		}),
	).Return(activities.ConfigureOpenVSwitchResult{Path: "rpc"}, nil).Once()

	env.ExecuteWorkflow(configureOpenVSwitchTestWorkflow, nodeID)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestConfigureCNIPrerequisitesRetriesOpenVSwitchAfterFiveSeconds(t *testing.T) {
	nodeID := uuid.New()
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	var a *activities.Activities
	env.OnActivity(a.GetClusterForNode, mock.Anything, nodeID).
		Return(db.Cluster{CniPlugin: db.CNIOvnKubernetes}, nil).Once()
	env.OnActivity(a.ConfigureOpenVSwitch, mock.Anything, mock.Anything).
		Return(activities.ConfigureOpenVSwitchResult{}, errors.New("connection lost after acceptance")).Once()
	env.OnActivity(a.ConfigureOpenVSwitch, mock.Anything, mock.Anything).
		Return(activities.ConfigureOpenVSwitchResult{Path: "rpc"}, nil).Once()

	var attemptStarts []time.Time
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == "ConfigureOpenVSwitch" {
			attemptStarts = append(attemptStarts, env.Now())
		}
	})

	env.ExecuteWorkflow(configureOpenVSwitchTestWorkflow, nodeID)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, attemptStarts, 2)
	require.Equal(t, 5*time.Second, attemptStarts[1].Sub(attemptStarts[0]))
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
