// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/temporal/activities"
)

func TestProvisionClusterMarksProvisioningFalseWhenTheWorkflowFails(t *testing.T) {
	clusterID := uuid.New()
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	a := &activities.Activities{}

	env.OnActivity(a.ApplyCAPIClusterResources, mock.Anything, mock.Anything).
		Return(temporal.NewNonRetryableApplicationError("capi apply failed", "Test", nil)).Once()
	env.OnActivity(a.UpdateClusterConditions, mock.Anything, mock.MatchedBy(func(u activities.ClusterConditionsUpdate) bool {
		p := u.Conditions.Provisioning
		return u.ClusterID == clusterID && p != nil && p.Status == db.ConditionStatusFalse &&
			strings.HasPrefix(p.Message, "Workflow failed: ")
	})).Return(nil).Once()

	env.ExecuteWorkflow(ProvisionCluster, ProvisionClusterInput{ClusterID: clusterID})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

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

func TestLabelSystemNamespacesForPSASchedulesActivityWithConditionMessage(t *testing.T) {
	clusterID := uuid.New()
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	a := &activities.Activities{}

	env.OnActivity(a.LabelSystemNamespacesForPSA, mock.Anything, activities.LabelSystemNamespacesForPSAInput{
		ClusterID:                           clusterID,
		ClusterProvisioningConditionMessage: "Labeling system namespaces for Pod Security Admission",
	}).Return(nil).Once()

	env.ExecuteWorkflow(labelSystemNamespacesForPSATestWorkflow, clusterID)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestLabelSystemNamespacesForPSARetriesAreBounded(t *testing.T) {
	clusterID := uuid.New()
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	a := &activities.Activities{}

	env.OnActivity(a.LabelSystemNamespacesForPSA, mock.Anything, mock.Anything).
		Return(errors.New("getting namespace kube-system: not found"))

	attempts := 0
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == "LabelSystemNamespacesForPSA" {
			attempts++
		}
	})

	env.ExecuteWorkflow(labelSystemNamespacesForPSATestWorkflow, clusterID)

	require.True(t, env.IsWorkflowCompleted())
	require.ErrorContains(t, env.GetWorkflowError(), "failed to label system namespaces for Pod Security Admission")
	require.Equal(t, 10, attempts)
}

func labelSystemNamespacesForPSATestWorkflow(ctx workflow.Context, clusterID uuid.UUID) error {
	return labelSystemNamespacesForPSA(ctx, clusterID, "Labeling system namespaces for Pod Security Admission")
}
