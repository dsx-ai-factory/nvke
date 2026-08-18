// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"

	"github.com/NVIDIA/nke/controller/internal/agent/agenttest"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestConfigureKubeVip_BGPPEERASNFromProvider(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		cluster  db.Cluster
		nodePool db.NodePool
		wantASN  string
	}{
		"CAPI pool without Nico payload uses default ASN": {
			cluster: db.Cluster{
				ProviderType:    db.ProviderTypeCAPI,
				ControlPlaneVIP: "10.0.0.10",
			},
			nodePool: db.NodePool{
				ControlPlane: true,
				Capi:         &db.CAPINodePoolProvider{},
			},
			wantASN: "4294967000",
		},
		"Nico pool with VPC prefix uses FMDS ASN": {
			cluster: db.Cluster{
				ProviderType:    db.ProviderTypeNico,
				ControlPlaneVIP: "10.0.0.10",
			},
			nodePool: db.NodePool{
				ControlPlane: true,
				Nico: &db.NicoNodePoolProvider{
					VpcPrefixIDs: []uuid.UUID{uuid.New()},
				},
			},
			wantASN: "FROM_FMDS",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			nodeID := uuid.New()
			nodeSetID := uuid.New()
			nodePoolID := uuid.New()
			clusterID := uuid.New()
			node := db.Node{
				ID:           nodeID,
				ClusterID:    clusterID,
				NodeSetID:    nodeSetID,
				NodePoolID:   nodePoolID,
				ControlPlane: true,
				IP:           "10.0.0.20",
			}

			tt.cluster.ID = clusterID
			tt.nodePool.ID = nodePoolID
			mockDB := &testutil.MockDB{}
			mockDB.On("GetNode", mock.Anything, nodeID).Return(node, nil).Times(3)
			mockDB.On("GetNodeSet", mock.Anything, nodeSetID).Return(db.NodeSet{ID: nodeSetID, NodePoolID: nodePoolID}, nil)
			mockDB.On("GetNodePool", mock.Anything, nodePoolID).Return(tt.nodePool, nil)
			mockDB.On("GetCluster", mock.Anything, clusterID).Return(tt.cluster, nil).Twice()

			fakeAgent := &agenttest.FakeClient{
				ExecuteScriptHook: func(_ db.Node, runbookName, script string) (*nkeproto.ExecuteScriptResponse, error) {
					require.Equal(t, "configure-kube-vip", runbookName)
					require.Contains(t, script, "export BGP_PEER_AS='"+tt.wantASN+"'")
					return &nkeproto.ExecuteScriptResponse{ExitCode: 0}, nil
				},
			}

			a := &Activities{db: mockDB, nodeAgentClient: fakeAgent}
			testSuite := &testsuite.WorkflowTestSuite{}
			env := testSuite.NewTestActivityEnvironment()
			env.RegisterActivity(a.configureKubeVip)
			_, err := env.ExecuteActivity(a.configureKubeVip, ConfigureKubeVipInput{NodeID: nodeID})
			require.NoError(t, err)
			mockDB.AssertExpectations(t)
		})
	}
}
