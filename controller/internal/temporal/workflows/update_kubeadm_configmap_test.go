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

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
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

	env.ExecuteWorkflow(UpdateKubeadmConfigMap, input)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result UpdateKubeadmConfigMapResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.True(t, result.Changed)
	env.AssertExpectations(t)
}
