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
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

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
