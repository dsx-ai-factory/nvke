// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

func TestRetireLostEtcdRotationNodeTearsDownOnlyTheRetiredNode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		teardown bool
	}{
		{name: "teardown required", teardown: true},
		{name: "node already removed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := RetireLostEtcdRotationNodeInput{ClusterID: uuid.New(), RotationWorkflowID: "owner", NodeID: uuid.New()}
			env, a := setupEnableEtcdEncryptionWorkflowEnv(t)
			env.RegisterWorkflow(TeardownNode)
			env.OnActivity(a.RetireLostEtcdRotationNode, mock.Anything, activities.RetireLostEtcdRotationNodeInput{
				ClusterID: input.ClusterID, RotationWorkflowID: input.RotationWorkflowID, NodeID: input.NodeID,
			}).Return(activities.RetireLostEtcdRotationNodeResult{TeardownRequired: tc.teardown}, nil).Once()
			var teardown []TeardownNodeInput
			if tc.teardown {
				env.OnWorkflow(TeardownNode, mock.Anything, mock.Anything).Return(func(_ workflow.Context, in TeardownNodeInput) error {
					teardown = append(teardown, in)
					return nil
				}).Once()
			}

			env.ExecuteWorkflow(RetireLostEtcdRotationNode, input)

			require.True(t, env.IsWorkflowCompleted())
			require.NoError(t, env.GetWorkflowError())
			if tc.teardown {
				require.Equal(t, []TeardownNodeInput{{
					NodeID: input.NodeID, AllowCPNode: true, ForceUnreachable: true, Reason: retiredEtcdRotationNodeReason,
				}}, teardown)
			} else {
				require.Empty(t, teardown)
			}
			env.AssertExpectations(t)
		})
	}
}

func TestRetireLostEtcdRotationNodeStopsOnRefusal(t *testing.T) {
	input := RetireLostEtcdRotationNodeInput{ClusterID: uuid.New(), RotationWorkflowID: "other", NodeID: uuid.New()}
	env, a := setupEnableEtcdEncryptionWorkflowEnv(t)
	env.RegisterWorkflow(TeardownNode)
	env.OnActivity(a.RetireLostEtcdRotationNode, mock.Anything, mock.Anything).Return(
		activities.RetireLostEtcdRotationNodeResult{},
		temporal.NewNonRetryableApplicationError("etcd key rotation does not own the cluster mutation lock", "EtcdEncryptionKeyRotationPrecondition", nil),
	).Once()

	env.ExecuteWorkflow(RetireLostEtcdRotationNode, input)

	require.True(t, env.IsWorkflowCompleted())
	require.ErrorContains(t, env.GetWorkflowError(), "does not own")
	env.AssertExpectations(t)
}

func TestRetireLostEtcdRotationNodeRequiresIdentifiers(t *testing.T) {
	env, _ := setupEnableEtcdEncryptionWorkflowEnv(t)
	env.ExecuteWorkflow(RetireLostEtcdRotationNode, RetireLostEtcdRotationNodeInput{ClusterID: uuid.New()})
	require.ErrorContains(t, env.GetWorkflowError(), "required")
}

// TestRetireLostEtcdRotationNodeReplacesCompletedTeardownRun starts the teardown
// child after a completed run of the same ID, as label-driven remediation leaves
// when it skips a control-plane node.
func TestRetireLostEtcdRotationNodeReplacesCompletedTeardownRun(t *testing.T) {
	input := RetireLostEtcdRotationNodeInput{ClusterID: uuid.New(), RotationWorkflowID: "owner", NodeID: uuid.New()}
	env, a := setupEnableEtcdEncryptionWorkflowEnv(t)
	env.RegisterWorkflow(TeardownNode)
	env.RegisterWorkflow(RetireLostEtcdRotationNode)
	env.OnActivity(a.RetireLostEtcdRotationNode, mock.Anything, mock.Anything).
		Return(activities.RetireLostEtcdRotationNodeResult{TeardownRequired: true}, nil).Once()
	var teardown []TeardownNodeInput
	env.OnWorkflow(TeardownNode, mock.Anything, mock.Anything).Return(func(_ workflow.Context, in TeardownNodeInput) error {
		teardown = append(teardown, in)
		return nil
	}).Twice()
	remediationThenRetire := func(ctx workflow.Context) error {
		childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			WorkflowID: constants.TeardownNodeWorkflowPrefix + input.NodeID.String(),
		})
		if err := workflow.ExecuteChildWorkflow(childCtx, TeardownNode, TeardownNodeInput{NodeID: input.NodeID}).Get(ctx, nil); err != nil {
			return err
		}
		return workflow.ExecuteChildWorkflow(ctx, RetireLostEtcdRotationNode, input).Get(ctx, nil)
	}

	env.ExecuteWorkflow(remediationThenRetire)

	require.NoError(t, env.GetWorkflowError())
	require.Len(t, teardown, 2)
	require.True(t, teardown[1].AllowCPNode)
	env.AssertExpectations(t)
}
