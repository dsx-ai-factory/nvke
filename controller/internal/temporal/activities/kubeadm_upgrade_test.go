// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"

	"github.com/NVIDIA/nke/controller/internal/agent/agenttest"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestKubeadmUpgradeApplyPrefersHealthyJoinedControlPlaneNode(t *testing.T) {
	clusterID := uuid.New()
	unhealthyNode := joinedControlPlaneNode(clusterID, "unhealthy", false)
	healthyNode := joinedControlPlaneNode(clusterID, "healthy", true)
	mockDB := &testutil.MockDB{}
	mockDB.On("ListNodes", mock.Anything, mock.MatchedBy(func(filters db.NodeFilters) bool {
		return filters.ClusterID != nil && *filters.ClusterID == clusterID &&
			filters.ControlPlane != nil && *filters.ControlPlane
	})).Return([]db.Node{unhealthyNode, healthyNode}, nil).Once()
	fakeAgent := &agenttest.FakeClient{
		KubeadmUpgradeApplyResponse: &nkeproto.KubeadmUpgradeApplyResponse{
			Outcome:       nkeproto.KubeadmUpgradeApplyResponse_OUTCOME_APPLIED,
			PlanOutput:    "plan output\n",
			ApplyOutput:   "apply output\n",
			TargetVersion: "v1.34.2",
		},
	}
	a := &Activities{db: mockDB, nodeAgentClient: fakeAgent}

	result, err := executeKubeadmUpgradeApplyActivity(t, a, KubeadmUpgradeApplyInput{
		ClusterID:         clusterID,
		KubernetesVersion: "1.34.2",
	})

	require.NoError(t, err)
	assert.Equal(t, healthyNode.ID, result.NodeID)
	assert.Equal(t, healthyNode.Name, result.NodeName)
	assert.Equal(t, "plan output\napply output\n", result.Output)
	require.Equal(t, []uuid.UUID{healthyNode.ID}, fakeAgent.KubeadmUpgradeApplyCalls)
	require.Len(t, fakeAgent.KubeadmUpgradeApplyRequests, 1)
	assert.Equal(t, "1.34.2", fakeAgent.KubeadmUpgradeApplyRequests[0].GetTargetVersion())
	mockDB.AssertExpectations(t)
}

func TestKubeadmUpgradeApplyFallsBackToJoinedControlPlaneNode(t *testing.T) {
	clusterID := uuid.New()
	joinedNode := joinedControlPlaneNode(clusterID, "joined", false)
	mockDB := &testutil.MockDB{}
	mockDB.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{joinedNode}, nil).Once()
	fakeAgent := &agenttest.FakeClient{
		KubeadmUpgradeApplyResponse: &nkeproto.KubeadmUpgradeApplyResponse{
			Outcome: nkeproto.KubeadmUpgradeApplyResponse_OUTCOME_APPLIED,
		},
	}
	a := &Activities{db: mockDB, nodeAgentClient: fakeAgent}

	result, err := executeKubeadmUpgradeApplyActivity(t, a, KubeadmUpgradeApplyInput{
		ClusterID:         clusterID,
		KubernetesVersion: "1.34.2",
	})

	require.NoError(t, err)
	assert.Equal(t, joinedNode.ID, result.NodeID)
	require.Equal(t, []uuid.UUID{joinedNode.ID}, fakeAgent.KubeadmUpgradeApplyCalls)
	mockDB.AssertExpectations(t)
}

func TestKubeadmUpgradeApplyReportsSelectionAndAgentFailures(t *testing.T) {
	clusterID := uuid.New()
	tests := []struct {
		name       string
		nodes      []db.Node
		agentError error
		response   *nkeproto.KubeadmUpgradeApplyResponse
		wantError  string
	}{
		{
			name:      "no joined control-plane nodes",
			nodes:     []db.Node{joinedControlPlaneNode(clusterID, "not-joined", true)},
			wantError: "no joined control plane nodes are available for kubeadm upgrade",
		},
		{
			name:       "agent failure",
			nodes:      []db.Node{joinedControlPlaneNode(clusterID, "healthy", true)},
			agentError: errors.New("agent unavailable"),
			wantError:  "failed to apply kubeadm upgrade on control plane node healthy",
		},
		{
			name:      "unspecified agent outcome",
			nodes:     []db.Node{joinedControlPlaneNode(clusterID, "healthy", true)},
			response:  &nkeproto.KubeadmUpgradeApplyResponse{},
			wantError: "returned outcome OUTCOME_UNSPECIFIED",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "no joined control-plane nodes" {
				test.nodes[0].Conditions.Joined.Status = db.ConditionStatusFalse
			}
			mockDB := &testutil.MockDB{}
			mockDB.On("ListNodes", mock.Anything, mock.Anything).Return(test.nodes, nil).Once()
			fakeAgent := &agenttest.FakeClient{
				KubeadmUpgradeApplyResponse: test.response,
				KubeadmUpgradeApplyError:    test.agentError,
			}
			a := &Activities{db: mockDB, nodeAgentClient: fakeAgent}

			_, err := executeKubeadmUpgradeApplyActivity(t, a, KubeadmUpgradeApplyInput{
				ClusterID:         clusterID,
				KubernetesVersion: "1.34.2",
			})

			require.ErrorContains(t, err, test.wantError)
			mockDB.AssertExpectations(t)
		})
	}
}

func joinedControlPlaneNode(clusterID uuid.UUID, name string, healthy bool) db.Node {
	node := db.Node{
		ID:           uuid.New(),
		ClusterID:    clusterID,
		Name:         name,
		ControlPlane: true,
		Conditions: db.NodeConditions{
			Joined: db.Condition{Status: db.ConditionStatusTrue},
		},
	}
	if healthy {
		node.Conditions.KubernetesReady.Status = db.ConditionStatusTrue
	}
	return node
}

func executeKubeadmUpgradeApplyActivity(
	t *testing.T,
	a *Activities,
	input KubeadmUpgradeApplyInput,
) (KubeadmUpgradeApplyResult, error) {
	t.Helper()
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.KubeadmUpgradeApply)
	value, err := env.ExecuteActivity(a.KubeadmUpgradeApply, input)
	if err != nil {
		return KubeadmUpgradeApplyResult{}, err
	}
	var result KubeadmUpgradeApplyResult
	if err := value.Get(&result); err != nil {
		return KubeadmUpgradeApplyResult{}, err
	}
	return result, nil
}
