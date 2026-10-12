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
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

func TestUpgradeControlPlaneAddonsUsesKubeadmUpgradeActivity(t *testing.T) {
	clusterID := uuid.New()
	nodeID := uuid.New()
	input := UpgradeControlPlaneAddonsInput{
		ClusterID:         clusterID,
		KubernetesVersion: "1.34.2",
	}

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	var a *activities.Activities
	env.OnActivity(
		a.KubeadmUpgradeApply,
		mock.Anything,
		activities.KubeadmUpgradeApplyInput{
			ClusterID:         clusterID,
			KubernetesVersion: "1.34.2",
		},
	).Return(activities.KubeadmUpgradeApplyResult{
		NodeID:   nodeID,
		NodeName: "cp-1",
		Output:   "plan\napply\n",
	}, nil).Once()
	env.OnActivity(
		a.InstallKubeLocalDNS,
		mock.Anything,
		activities.InstallKubeLocalDNSInput{ClusterID: clusterID},
	).Return(nil).Once()
	env.OnActivity(
		a.LabelSystemNamespacesForPSA,
		mock.Anything,
		activities.LabelSystemNamespacesForPSAInput{ClusterID: clusterID},
	).Return(nil).Once()

	var heartbeatTimeout time.Duration
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == "KubeadmUpgradeApply" {
			heartbeatTimeout = info.HeartbeatTimeout
		}
	})

	env.ExecuteWorkflow(UpgradeControlPlaneAddons, input)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result UpgradeControlPlaneAddonsResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, UpgradeControlPlaneAddonsResult{
		NodeID:   nodeID,
		NodeName: "cp-1",
		Output:   "plan\napply\n",
	}, result)
	require.Equal(t, defaultRunbookActivityHeartbeatTimeout(), heartbeatTimeout)
	env.AssertExpectations(t)
}

// An upgrade that started before LabelSystemNamespacesForPSA existed has no
// marker for it in its history, so replay must not schedule it.
func TestUpgradeControlPlaneAddonsSkipsPSALabelingOnOldHistory(t *testing.T) {
	clusterID := uuid.New()
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	var a *activities.Activities
	env.OnActivity(a.KubeadmUpgradeApply, mock.Anything, mock.Anything).
		Return(activities.KubeadmUpgradeApplyResult{}, nil).Once()
	env.OnActivity(a.InstallKubeLocalDNS, mock.Anything, mock.Anything).Return(nil).Once()
	env.OnGetVersion(psaLabelSystemNamespacesChangeID, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)

	labeled := false
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == "LabelSystemNamespacesForPSA" {
			labeled = true
		}
	})

	env.ExecuteWorkflow(UpgradeControlPlaneAddons, UpgradeControlPlaneAddonsInput{
		ClusterID:         clusterID,
		KubernetesVersion: "1.34.2",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.False(t, labeled)
	env.AssertExpectations(t)
}
