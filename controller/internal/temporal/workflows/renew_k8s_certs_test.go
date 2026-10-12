// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

func TestRenewK8sCertsUsesTypedActivitySequentially(t *testing.T) {
	clusterID := uuid.New()
	nodes := []db.Node{
		{ID: uuid.New(), Name: "cp-0", ClusterID: clusterID, ControlPlane: true},
		{ID: uuid.New(), Name: "cp-1", ClusterID: clusterID, ControlPlane: true},
	}
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	var a *activities.Activities
	env.OnActivity(a.ListControlPlaneNodes, mock.Anything, clusterID).Return(nodes, nil).Once()
	env.OnActivity(a.RenewKubernetesCertificates, mock.Anything, mock.Anything).
		Return(activities.KubernetesCertificateRenewalResult{Output: "typed success"}, nil).
		Twice()

	var inputs []activities.KubernetesCertificateRenewalInput
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, values converter.EncodedValues) {
		if info.ActivityType.Name != "RenewKubernetesCertificates" {
			return
		}
		var input activities.KubernetesCertificateRenewalInput
		require.NoError(t, values.Get(&input))
		inputs = append(inputs, input)
	})

	env.ExecuteWorkflow(RenewK8sCerts, RenewK8sCertsInput{ClusterID: clusterID, ThresholdDays: 7})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, inputs, 2)
	for index, input := range inputs {
		require.Equal(t, nodes[index].ID, input.NodeID)
		require.Equal(t, 7, input.ThresholdDays)
		require.True(t, strings.HasPrefix(input.OperationID, "k8s-cert-renewal/"))
		require.True(t, strings.HasSuffix(input.OperationID, "/"+nodes[index].ID.String()))
	}
	require.NotEqual(t, inputs[0].OperationID, inputs[1].OperationID)

	var result RenewK8sCertsResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, []RenewK8sCertsNodeResult{
		{NodeID: nodes[0].ID, NodeName: nodes[0].Name, Output: "typed success"},
		{NodeID: nodes[1].ID, NodeName: nodes[1].Name, Output: "typed success"},
	}, result.Results)
	env.AssertExpectations(t)
}
