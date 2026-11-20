// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

var errAgentUnreachable = errors.New("connection refused")

// initTestNode returns a control-plane node of clusterID with the given conditions.
func initTestNode(clusterID uuid.UUID, name string, readyToJoin, joined bool) db.Node {
	return db.Node{
		ID:           uuid.New(),
		ClusterID:    clusterID,
		ControlPlane: true,
		Name:         name,
		Conditions: db.NodeConditions{
			ReadyToJoin: db.NewCondition(readyToJoin, ""),
			Joined:      db.NewCondition(joined, ""),
		},
	}
}

// kubeadmStatusAgent reports per-node kubeadm state and records which nodes it was asked about.
type kubeadmStatusAgent struct {
	statuses map[uuid.UUID]*nvkeproto.KubeadmStatusResponse
	errs     map[uuid.UUID]error
	probed   []string
}

func (s *kubeadmStatusAgent) client() *agenttest.FakeClient {
	return &agenttest.FakeClient{GetKubeadmStatusHook: func(node db.Node) (*nvkeproto.KubeadmStatusResponse, error) {
		s.probed = append(s.probed, node.Name)
		if err := s.errs[node.ID]; err != nil {
			return nil, err
		}
		if status := s.statuses[node.ID]; status != nil {
			return status, nil
		}
		return &nvkeproto.KubeadmStatusResponse{}, nil
	}}
}

func initPhase(status string) *nvkeproto.KubeadmStatusResponse {
	return &nvkeproto.KubeadmStatusResponse{KubeadmPhase: kubeadmPhaseInit, KubeadmStatus: status}
}

func controlPlaneNodesDatabase(clusterID uuid.UUID, nodes []db.Node, err error) *testutil.MockDB {
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, mock.MatchedBy(func(filters db.NodeFilters) bool {
		return filters.ClusterID != nil && *filters.ClusterID == clusterID &&
			filters.ControlPlane != nil && *filters.ControlPlane
	})).Return(nodes, err)
	return database
}

func TestWaitForControlPlaneNodeReadyToJoinChoosesInitialNode(t *testing.T) {
	clusterID := uuid.New()
	provisioning := initTestNode(clusterID, "cp-provisioning", false, false)
	ready := initTestNode(clusterID, "cp-ready", true, false)
	unreachable := initTestNode(clusterID, "cp-unreachable", true, false)
	initialized := initTestNode(clusterID, "cp-initialized", true, true)

	tests := []struct {
		name       string
		nodes      []db.Node
		statuses   map[uuid.UUID]*nvkeproto.KubeadmStatusResponse
		errs       map[uuid.UUID]error
		want       db.Node
		wantProbed []string
	}{
		{
			name:       "first ready node when kubeadm init ran nowhere",
			nodes:      []db.Node{provisioning, ready, unreachable},
			want:       ready,
			wantProbed: []string{"cp-ready", "cp-unreachable"},
		},
		{
			name:       "node kubeadm init ran on, even when another ready node comes first",
			nodes:      []db.Node{ready, initialized},
			statuses:   map[uuid.UUID]*nvkeproto.KubeadmStatusResponse{initialized.ID: initPhase(kubeadmStatusReady)},
			want:       initialized,
			wantProbed: []string{"cp-ready", "cp-initialized"},
		},
		{
			name:       "node whose kubeadm init is still running",
			nodes:      []db.Node{ready, unreachable},
			statuses:   map[uuid.UUID]*nvkeproto.KubeadmStatusResponse{unreachable.ID: initPhase(kubeadmStatusPending)},
			want:       unreachable,
			wantProbed: []string{"cp-ready", "cp-unreachable"},
		},
		{
			name:       "unreachable agent does not stop the search",
			nodes:      []db.Node{unreachable, initialized},
			statuses:   map[uuid.UUID]*nvkeproto.KubeadmStatusResponse{initialized.ID: initPhase(kubeadmStatusReady)},
			errs:       map[uuid.UUID]error{unreachable.ID: errAgentUnreachable},
			want:       initialized,
			wantProbed: []string{"cp-unreachable", "cp-initialized"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &kubeadmStatusAgent{statuses: tt.statuses, errs: tt.errs}
			a := &Activities{db: controlPlaneNodesDatabase(clusterID, tt.nodes, nil), nodeAgentClient: agent.client()}

			env := (&testsuite.WorkflowTestSuite{}).NewTestActivityEnvironment()
			env.RegisterActivity(a.waitForControlPlaneNodeReadyToJoin)
			result, err := env.ExecuteActivity(a.waitForControlPlaneNodeReadyToJoin, WaitForControlPlaneNodeReadyToJoinInput{ClusterID: clusterID})
			require.NoError(t, err)

			var got WaitForControlPlaneNodeReadyToJoinResult
			require.NoError(t, result.Get(&got))
			assert.Equal(t, tt.want.ID, got.NodeID)
			assert.Equal(t, tt.wantProbed, agent.probed, "only nodes at or past ReadyToJoin are asked")
		})
	}
}

func TestRequireNoOtherInitializedControlPlaneNode(t *testing.T) {
	clusterID := uuid.New()
	initial := initTestNode(clusterID, "cp-initial", true, false)
	provisioning := initTestNode(clusterID, "cp-provisioning", false, false)
	ready := initTestNode(clusterID, "cp-ready", true, false)
	joined := initTestNode(clusterID, "cp-joined", true, true)

	tests := []struct {
		name             string
		nodes            []db.Node
		listErr          error
		statuses         map[uuid.UUID]*nvkeproto.KubeadmStatusResponse
		errs             map[uuid.UUID]error
		wantErr          string
		wantNonRetryable bool
		wantProbed       []string
	}{
		{
			name:  "initial node is the only control plane node",
			nodes: []db.Node{initial},
		},
		{
			name:       "other nodes have not run kubeadm",
			nodes:      []db.Node{initial, provisioning, ready},
			errs:       map[uuid.UUID]error{provisioning.ID: errAgentUnreachable},
			wantProbed: []string{"cp-ready"},
		},
		{
			name:             "another node has joined",
			nodes:            []db.Node{initial, joined},
			wantErr:          "control plane node cp-joined has already joined the cluster, so kubeadm init must not run on node cp-initial",
			wantNonRetryable: true,
		},
		{
			name:             "another node reports kubeadm init",
			nodes:            []db.Node{initial, ready},
			statuses:         map[uuid.UUID]*nvkeproto.KubeadmStatusResponse{ready.ID: initPhase(kubeadmStatusError)},
			wantErr:          "kubeadm init already ran on control plane node cp-ready, so it must not run on node cp-initial",
			wantNonRetryable: true,
			wantProbed:       []string{"cp-ready"},
		},
		{
			name:       "another node's agent cannot be reached",
			nodes:      []db.Node{initial, ready},
			errs:       map[uuid.UUID]error{ready.ID: errAgentUnreachable},
			wantErr:    "cannot confirm that kubeadm init has not run on control plane node cp-ready",
			wantProbed: []string{"cp-ready"},
		},
		{
			name:    "listing the control plane fails",
			listErr: errors.New("database unavailable"),
			wantErr: "failed to list control plane nodes: database unavailable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &kubeadmStatusAgent{statuses: tt.statuses, errs: tt.errs}
			a := &Activities{db: controlPlaneNodesDatabase(clusterID, tt.nodes, tt.listErr), nodeAgentClient: agent.client()}

			err := a.requireNoOtherInitializedControlPlaneNode(context.Background(), clusterID, initial)

			assert.Equal(t, tt.wantProbed, agent.probed)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
			var applicationErr *temporal.ApplicationError
			if tt.wantNonRetryable {
				require.ErrorAs(t, err, &applicationErr)
				assert.True(t, applicationErr.NonRetryable())
				assert.Equal(t, errTypeControlPlaneAlreadyInitialized, applicationErr.Type())
				return
			}
			assert.False(t, errors.As(err, &applicationErr) && applicationErr.NonRetryable(), "the workflow must keep retrying")
		})
	}
}

func TestKubeadmInitRefusesSecondInitialNode(t *testing.T) {
	cluster, initialNode, nodeSet, nodePool := snapshotInitRecords()
	joined := initTestNode(cluster.ID, "cp-joined", true, true)
	database := &testutil.MockDB{MockTx: snapshotAdmissionTx(cluster, initialNode)}
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
	database.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil).Once()
	database.On("GetNode", mock.Anything, initialNode.ID).Return(initialNode, nil).Once()
	database.On("GetNodeSet", mock.Anything, nodeSet.ID).Return(nodeSet, nil).Once()
	database.On("GetNodePool", mock.Anything, nodePool.ID).Return(nodePool, nil).Once()
	database.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{initialNode, joined}, nil).Once()
	agent := &snapshotWiringAgent{FakeClient: &agenttest.FakeClient{}, initStatus: true}
	a := snapshotActivities(database, agent, snapshotSharedConfig())

	err := executeSnapshotInit(t, a, KubeadmInitInput{ClusterID: cluster.ID, InitialNodeID: initialNode.ID})

	require.ErrorContains(t, err, "control plane node cp-joined has already joined the cluster")
	assert.Empty(t, agent.KubeadmFilesSupportCalls, "no kubeadm files are sent to a second node")
	assert.Empty(t, agent.initRequests, "kubeadm init must not run on a second node")
	database.AssertExpectations(t)
}
