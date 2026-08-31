// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"

	"github.com/NVIDIA/nke/controller/internal/agent/agenttest"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestConfigureKubeVip_BGPPEERASNFromProvider(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		cluster    db.Cluster
		nodePool   db.NodePool
		wantSource nkeproto.ReconcileKubeVipRequest_BGPPeerASNSource
		wantASN    uint32
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
			wantSource: nkeproto.ReconcileKubeVipRequest_BGP_PEER_ASN_SOURCE_STATIC,
			wantASN:    4294967000,
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
			wantSource: nkeproto.ReconcileKubeVipRequest_BGP_PEER_ASN_SOURCE_FMDS,
			wantASN:    0,
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
			mockDB.On("GetNode", mock.Anything, nodeID).Return(node, nil).Twice()
			mockDB.On("GetNodeSet", mock.Anything, nodeSetID).Return(db.NodeSet{ID: nodeSetID, NodePoolID: nodePoolID}, nil)
			mockDB.On("GetNodePool", mock.Anything, nodePoolID).Return(tt.nodePool, nil)
			mockDB.On("GetCluster", mock.Anything, clusterID).Return(tt.cluster, nil).Once()

			fakeAgent := &agenttest.FakeClient{
				ReconcileKubeVipResponse: &nkeproto.ReconcileKubeVipResponse{
					Outcome:           nkeproto.ReconcileKubeVipResponse_OUTCOME_UNCHANGED,
					ManifestPath:      "/etc/kubernetes/manifests/kube-vip.yaml",
					BgpInterface:      "eth0",
					BgpPeerAddress:    "10.0.0.1",
					BgpPeerAsn:        tt.wantASN,
					KubeconfigPath:    "/etc/kubernetes/kube-vip.conf",
					KubeconfigChanged: false,
				},
			}

			a := &Activities{db: mockDB, nodeAgentClient: fakeAgent}
			testSuite := &testsuite.WorkflowTestSuite{}
			env := testSuite.NewTestActivityEnvironment()
			env.RegisterActivity(a.configureKubeVip)
			value, err := env.ExecuteActivity(a.configureKubeVip, ConfigureKubeVipInput{
				NodeID:              nodeID,
				ConfigureSuperAdmin: true,
			})
			require.NoError(t, err)
			var result ConfigureKubeVipResult
			require.NoError(t, value.Get(&result))
			require.Nil(t, result.ConfigureSuperAdminOutput)
			require.Contains(t, result.ConfigureKubeVipOutput, "OUTCOME_UNCHANGED")
			require.Len(t, fakeAgent.ReconcileKubeVipRequests, 1)
			request := fakeAgent.ReconcileKubeVipRequests[0]
			require.Equal(t, "10.0.0.10", request.GetVipAddress())
			require.Equal(t, "10.0.0.20", request.GetNodeIp())
			require.Equal(t, uint32(65100), request.GetBgpAsn())
			require.Equal(t, tt.wantSource, request.GetBgpPeerAsnSource())
			require.Equal(t, tt.wantASN, request.GetBgpPeerAsn())
			mockDB.AssertExpectations(t)
		})
	}
}

func TestConfigureKubeVip_RPCFallbackBoundary(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		rpcError            error
		configureSuperAdmin bool
		wantRunbooks        []string
		wantError           string
	}{
		"unimplemented uses kube-vip runbook": {
			rpcError:     status.Error(codes.Unimplemented, "unknown method ReconcileKubeVip"),
			wantRunbooks: []string{configureKubeVipRunbookName},
		},
		"wrapped unimplemented preserves legacy super-admin ordering": {
			rpcError:            fmt.Errorf("reconcile call: %w", status.Error(codes.Unimplemented, "unknown method ReconcileKubeVip")),
			configureSuperAdmin: true,
			wantRunbooks:        []string{"configure-super-admin", configureKubeVipRunbookName},
		},
		"operational RPC error is propagated": {
			rpcError:  status.Error(codes.Internal, "manifest write failed"),
			wantError: "manifest write failed",
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
				Name:         "cp-0",
				ClusterID:    clusterID,
				NodeSetID:    nodeSetID,
				NodePoolID:   nodePoolID,
				ControlPlane: true,
				IP:           "10.0.0.20",
			}
			cluster := db.Cluster{ID: clusterID, ControlPlaneVIP: "10.0.0.10"}
			nodePool := db.NodePool{ID: nodePoolID, ControlPlane: true, Capi: &db.CAPINodePoolProvider{}}
			mockDB := &testutil.MockDB{}
			mockDB.On("GetNode", mock.Anything, nodeID).Return(node, nil).Times(2 + len(tt.wantRunbooks))
			mockDB.On("GetNodeSet", mock.Anything, nodeSetID).Return(db.NodeSet{ID: nodeSetID, NodePoolID: nodePoolID}, nil)
			mockDB.On("GetNodePool", mock.Anything, nodePoolID).Return(nodePool, nil)
			mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Times(1 + len(tt.wantRunbooks))

			fakeAgent := &agenttest.FakeClient{ReconcileKubeVipError: tt.rpcError}
			fakeAgent.ExecuteScriptHook = func(_ db.Node, runbookName, script string) (*nkeproto.ExecuteScriptResponse, error) {
				if runbookName == configureKubeVipRunbookName {
					require.Contains(t, script, "export VIP_ADDRESS='10.0.0.10'")
					require.Contains(t, script, "export BGP_AS='65100'")
					require.Contains(t, script, "export BGP_PEER_AS='4294967000'")
					require.Contains(t, script, "export NODE_IP='10.0.0.20'")
				}
				return &nkeproto.ExecuteScriptResponse{ExitCode: 0, Output: runbookName + " output"}, nil
			}

			a := &Activities{db: mockDB, nodeAgentClient: fakeAgent}
			testSuite := &testsuite.WorkflowTestSuite{}
			env := testSuite.NewTestActivityEnvironment()
			env.RegisterActivity(a.configureKubeVip)
			value, err := env.ExecuteActivity(a.configureKubeVip, ConfigureKubeVipInput{
				NodeID:              nodeID,
				ConfigureSuperAdmin: tt.configureSuperAdmin,
			})
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
				require.Empty(t, fakeAgent.ExecuteScriptNames)
			} else {
				require.NoError(t, err)
				var result ConfigureKubeVipResult
				require.NoError(t, value.Get(&result))
				require.Equal(t, tt.wantRunbooks, fakeAgent.ExecuteScriptNames)
				require.Equal(t, configureKubeVipRunbookName+" output", result.ConfigureKubeVipOutput)
				if tt.configureSuperAdmin {
					require.NotNil(t, result.ConfigureSuperAdminOutput)
					require.Equal(t, "configure-super-admin output", *result.ConfigureSuperAdminOutput)
				} else {
					require.Nil(t, result.ConfigureSuperAdminOutput)
				}
			}
			require.Len(t, fakeAgent.ReconcileKubeVipRequests, 1)
			mockDB.AssertExpectations(t)
		})
	}
}
