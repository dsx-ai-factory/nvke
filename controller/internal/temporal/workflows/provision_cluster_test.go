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

	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

func TestInstallKubeLocalDNSSchedulesInProcessActivity(t *testing.T) {
	clusterID := uuid.New()
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	a := &activities.Activities{}

	env.OnActivity(a.InstallKubeLocalDNS, mock.Anything, activities.InstallKubeLocalDNSInput{
		ClusterID:                           clusterID,
		ClusterProvisioningConditionMessage: "Installing kube-local DNS",
	}).Return(nil).Once()

	env.ExecuteWorkflow(installKubeLocalDNSTestWorkflow, clusterID)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func installKubeLocalDNSTestWorkflow(ctx workflow.Context, clusterID uuid.UUID) error {
	return installKubeLocalDNS(ctx, clusterID)
}
