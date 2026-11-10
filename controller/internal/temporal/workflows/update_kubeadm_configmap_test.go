// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

func TestUpdateKubeadmConfigMapUsesTenantClientActivity(t *testing.T) {
	clusterID := uuid.New()
	input := UpdateKubeadmConfigMapInput{
		ClusterID:         clusterID,
		KubernetesVersion: "v1.31.4",
	}

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	a := &activities.Activities{}
	env.OnActivity(
		a.UpdateKubeadmKubernetesVersion,
		mock.Anything,
		activities.UpdateKubeadmKubernetesVersionInput{
			ClusterID:         clusterID,
			KubernetesVersion: "v1.31.4",
		},
	).Return(activities.UpdateKubeadmKubernetesVersionResult{Changed: true}, nil).Once()
	env.OnActivity(
		a.MergeEtcdDefragKubeadmConfig,
		mock.Anything,
		activities.KubeadmConfigClusterInput{ClusterID: clusterID},
	).Return(activities.MergeEtcdDefragKubeadmConfigResult{Changed: false}, nil).Once()

	env.ExecuteWorkflow(UpdateKubeadmConfigMap, input)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result UpdateKubeadmConfigMapResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.True(t, result.Changed)
	env.AssertExpectations(t)
}

// The etcd merge gates the defragmentation threshold on the kubernetesVersion
// already recorded in the ConfigMap, so running it first would read the old
// version and skip the threshold on exactly the upgrade that makes the cluster
// eligible for it. The ordering is the contract, not an implementation detail.
func TestUpdateKubeadmConfigMapMergesEtcdSettingsAfterTheVersion(t *testing.T) {
	clusterID := uuid.New()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	a := &activities.Activities{}

	// Registered in the opposite order to the one expected, so a slice built at
	// registration rather than at execution would come out reversed and fail.
	var order []string
	env.OnActivity(a.MergeEtcdDefragKubeadmConfig, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { order = append(order, "etcd-defrag") }).
		Return(activities.MergeEtcdDefragKubeadmConfigResult{Changed: true}, nil).Once()
	env.OnActivity(a.UpdateKubeadmKubernetesVersion, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { order = append(order, "version") }).
		Return(activities.UpdateKubeadmKubernetesVersionResult{Changed: false}, nil).Once()

	env.ExecuteWorkflow(UpdateKubeadmConfigMap, UpdateKubeadmConfigMapInput{
		ClusterID:         clusterID,
		KubernetesVersion: "v1.34.2",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []string{"version", "etcd-defrag"}, order)

	// The etcd merge alone is still a change: on a cluster already at the
	// target version the threshold is the only thing that moves.
	var result UpdateKubeadmConfigMapResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.True(t, result.Changed)
	env.AssertExpectations(t)
}
