// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestTriggerClusterTeardownControlPlaneNodes_OnlyClusterLevelNodes(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	cpNode := db.Node{ID: uuid.New(), ClusterID: clusterID, ControlPlane: true, NodePoolID: uuid.Nil}
	// A nodepool-managed control-plane node must be left to teardownNodePools.
	poolNode := db.Node{ID: uuid.New(), ClusterID: clusterID, ControlPlane: true, NodePoolID: uuid.New()}

	mockDB := &testutil.MockDB{}
	mockTx := &testutil.MockTx{}
	mockDB.MockTx = mockTx
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	mockTx.On("ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
		return f.ClusterID != nil && *f.ClusterID == clusterID && f.ControlPlane != nil && *f.ControlPlane
	})).Return([]db.Node{cpNode, poolNode}, nil)
	mockTx.On("GetNodeForUpdate", mock.Anything, cpNode.ID).Return(cpNode, nil).Once()
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(n *db.Node) bool {
		return n.ID == cpNode.ID && n.Conditions.Deleting.Status == db.ConditionStatusTrue
	})).Return(cpNode, nil).Once()
	mockTx.On("CreateWorkflowOutboxEntry", mock.Anything,
		constants.TeardownNodeWorkflowType,
		constants.TeardownNodeWorkflowPrefix+cpNode.ID.String(),
		mock.MatchedBy(func(input interface{}) bool {
			payload, ok := input.(map[string]interface{})
			return ok &&
				payload["nodeID"] == cpNode.ID &&
				payload["skipRemoveFromCluster"] == true &&
				payload["allowCPNode"] == true
		}),
	).Return(db.WorkflowOutbox{}, nil).Once()

	a := &Activities{db: mockDB}
	result, err := executeTriggerClusterTeardownControlPlaneNodes(t, a,
		TriggerClusterTeardownControlPlaneNodesInput{ClusterID: clusterID})
	require.NoError(t, err)

	assert.Equal(t, []uuid.UUID{cpNode.ID}, result.NodeIDs)
	mockTx.AssertExpectations(t)
	mockTx.AssertNotCalled(t, "GetNodeForUpdate", mock.Anything, poolNode.ID)
}

func executeTriggerClusterTeardownControlPlaneNodes(
	t *testing.T,
	a *Activities,
	input TriggerClusterTeardownControlPlaneNodesInput,
) (TriggerClusterTeardownControlPlaneNodesResult, error) {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.triggerClusterTeardownControlPlaneNodes)

	val, err := env.ExecuteActivity(a.triggerClusterTeardownControlPlaneNodes, input)
	if err != nil {
		return TriggerClusterTeardownControlPlaneNodesResult{}, err
	}

	var result TriggerClusterTeardownControlPlaneNodesResult
	require.NoError(t, val.Get(&result))
	return result, nil
}
