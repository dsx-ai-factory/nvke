// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

func TestNodeReconfigureKubeletServerActivityOrder(t *testing.T) {
	nodeID := uuid.New()
	server := "https://api.example.com:6443"
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	var a *activities.Activities
	env.OnActivity(
		a.TaintNode,
		mock.Anything,
		activities.TaintNodeInput{
			NodeID:      nodeID,
			TaintKey:    keys.MaintenanceTaint.String(),
			TaintValue:  kubeVIPMaintenanceTaintValue,
			TaintEffect: constants.TaintEffectNoSchedule,
		},
	).Return(activities.TaintNodeResult{}, nil).Once()
	env.OnActivity(
		a.ReconfigureKubeletAPIServer,
		mock.Anything,
		activities.ReconfigureKubeletAPIServerInput{NodeID: nodeID, Server: server},
	).Return(activities.ReconfigureKubeletAPIServerResult{}, nil).Once()
	env.OnActivity(
		a.WaitForNodeLeaseRenewal,
		mock.Anything,
		activities.WaitForNodeLeaseRenewalInput{NodeID: nodeID},
	).Return(activities.WaitForNodeLeaseRenewalResult{}, nil).Once()
	env.OnActivity(
		a.TaintNode,
		mock.Anything,
		activities.TaintNodeInput{
			NodeID:      nodeID,
			TaintKey:    keys.MaintenanceTaint.String(),
			TaintValue:  kubeVIPMaintenanceTaintValue,
			TaintEffect: constants.TaintEffectNoSchedule + "-",
		},
	).Return(activities.TaintNodeResult{}, nil).Once()

	var activityOrder []string
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		activityOrder = append(activityOrder, info.ActivityType.Name)
	})

	env.ExecuteWorkflow(NodeReconfigureKubeletServer, nodeID, NodeReconfigureKubeletServerInput{Server: server})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []string{
		"TaintNode",
		"ReconfigureKubeletAPIServer",
		"WaitForNodeLeaseRenewal",
		"TaintNode",
	}, activityOrder)
	env.AssertExpectations(t)
}

func TestWaitForKubeletToRenewNodeLeaseHasHeartbeatTimeout(t *testing.T) {
	nodeID := uuid.New()
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	var a *activities.Activities
	env.OnActivity(
		a.WaitForNodeLeaseRenewal,
		mock.Anything,
		activities.WaitForNodeLeaseRenewalInput{NodeID: nodeID},
	).Return(activities.WaitForNodeLeaseRenewalResult{}, nil).Once()

	var heartbeatTimeout time.Duration
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == "WaitForNodeLeaseRenewal" {
			heartbeatTimeout = info.HeartbeatTimeout
		}
	})

	env.ExecuteWorkflow(waitForKubeletToRenewNodeLease, nodeID)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 30*time.Second, heartbeatTimeout)
	env.AssertExpectations(t)
}
