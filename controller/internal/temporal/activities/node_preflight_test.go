// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestNodePreflight(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name                     string
		siteID                   uuid.UUID
		capiInfraCluster         json.RawMessage
		capiInfraMachineTemplate json.RawMessage
		agentVersion             string
		probedAgentVersion       string
		probeErr                 error
		healthChecksDisabled     bool
		preflightErr             error
		preflightStatus          string
		preflightErrorMessage    string
		wantErr                  string
		wantBenignError          bool
		wantOrdinaryError        bool
		wantPreflightCalls       int
		wantVersionCalls         int
		assertPreflightRequest   func(t *testing.T, req *nvkeproto.ExecuteNodePreflightRequest)
	}{
		{
			name: "health checks disabled discovers missing worker version", siteID: uuid.New(),
			healthChecksDisabled: true, probedAgentVersion: "0.1.7", wantVersionCalls: 1, wantPreflightCalls: 1,
		},
		{
			name: "blank stored version discovers current agent", siteID: uuid.New(), agentVersion: "  ",
			probedAgentVersion: "0.1.24", wantVersionCalls: 1, wantPreflightCalls: 1,
		},
		{
			name: "discovered legacy version still waits for bootstrap", siteID: uuid.New(),
			probedAgentVersion: "0.1.22", wantVersionCalls: 1, wantPreflightCalls: 1,
			preflightStatus: "notready", wantErr: "notready", wantBenignError: true,
		},
		{
			name: "empty probe version remains pending", siteID: uuid.New(), wantVersionCalls: 1,
			wantErr: "version is not populated", wantBenignError: true,
		},
		{
			name: "blank probe version remains pending", siteID: uuid.New(), probedAgentVersion: "  ", wantVersionCalls: 1,
			wantErr: "version is not populated", wantBenignError: true,
		},
		{
			name: "version probe failure retries without preflight", siteID: uuid.New(), wantVersionCalls: 1,
			probeErr: errors.New("version probe failed"), wantErr: "version probe failed", wantOrdinaryError: true,
		},
		{
			name: "version probe preserves cancellation", siteID: uuid.New(), wantVersionCalls: 1,
			probeErr: context.Canceled, wantErr: context.Canceled.Error(), wantOrdinaryError: true,
		},
		{
			name: "version probe preserves deadline", siteID: uuid.New(), wantVersionCalls: 1,
			probeErr: context.DeadlineExceeded, wantErr: context.DeadlineExceeded.Error(), wantOrdinaryError: true,
		},
		{name: "malformed version still checks", siteID: uuid.New(), agentVersion: "unknown", wantPreflightCalls: 1},
		{
			name:                     "old agent still requires bootstrap completion",
			siteID:                   uuid.New(),
			capiInfraCluster:         json.RawMessage(`{}`),
			capiInfraMachineTemplate: json.RawMessage(`{}`),
			agentVersion:             "0.1.22",
			wantPreflightCalls:       1,
			preflightStatus:          "notready",
			wantErr:                  "notready",
			wantBenignError:          true,
		},
		{
			name:                     "Forge node with current agent checks cloud-init status",
			siteID:                   uuid.New(),
			capiInfraCluster:         json.RawMessage(`{}`),
			capiInfraMachineTemplate: json.RawMessage(`{}`),
			agentVersion:             "0.1.24",
			wantPreflightCalls:       1,
			assertPreflightRequest: func(t *testing.T, req *nvkeproto.ExecuteNodePreflightRequest) {
				t.Helper()
				assert.NotNil(t, req.GetCheckCloudInitStatus())
				assert.Empty(t, req.GetChecks())
			},
		},
		{
			name:             "CAPD node with current agent checks CAPD bootstrap sentinel",
			capiInfraCluster: json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta2","kind":"DockerCluster"}`),
			capiInfraMachineTemplate: json.RawMessage(
				`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta2","kind":"DockerMachineTemplate"}`,
			),
			agentVersion:       "0.1.24",
			wantPreflightCalls: 1,
			assertPreflightRequest: func(t *testing.T, req *nvkeproto.ExecuteNodePreflightRequest) {
				t.Helper()
				assert.Nil(t, req.GetCheckCloudInitStatus())
				checks := req.GetChecks()
				require.Len(t, checks, 1)
				assert.Equal(t, "cluster-api-bootstrap-success", checks[0].GetName())
				assert.Equal(t, capdBootstrapSuccessPath, checks[0].GetFileExists().GetPath())
			},
		},
		{
			name:                  "preflight not ready is an expected retry",
			siteID:                uuid.New(),
			agentVersion:          "0.1.24",
			preflightStatus:       nodePreflightStatusNotReady,
			preflightErrorMessage: "cloud-init is still running",
			wantErr:               `node preflight status "notready": cloud-init is still running`,
			wantBenignError:       true,
			wantPreflightCalls:    1,
		},
		{
			name:                  "preflight error status remains an ordinary error",
			siteID:                uuid.New(),
			agentVersion:          "0.1.24",
			preflightStatus:       "error",
			preflightErrorMessage: "cloud-init failed",
			wantErr:               `node preflight status "error": cloud-init failed`,
			wantOrdinaryError:     true,
			wantPreflightCalls:    1,
		},
		{
			name:               "unexpected preflight RPC failure remains an ordinary error",
			siteID:             uuid.New(),
			agentVersion:       "0.1.24",
			preflightErr:       errors.New("unexpected RPC failure"),
			wantErr:            "unexpected RPC failure",
			wantOrdinaryError:  true,
			wantPreflightCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			activity, node := setupNodePreflightRequestTest(t, ctx, tt.siteID, tt.capiInfraCluster, tt.capiInfraMachineTemplate)
			activity.cfg.NodeHealthCheckDisabled = tt.healthChecksDisabled
			node.AgentVersion = tt.agentVersion
			_, updateErr := activity.db.UpdateNode(ctx, &node)
			require.NoError(t, updateErr)
			preflightStatus := tt.preflightStatus
			if preflightStatus == "" {
				preflightStatus = nodePreflightStatusReady
			}
			nodeAgent := &agenttest.FakeClient{
				AgentVersion:    tt.probedAgentVersion,
				AgentVersionErr: tt.probeErr,
				PreflightErr:    tt.preflightErr,
				PreflightResp: &nvkeproto.ExecuteNodePreflightResponse{
					Status:       preflightStatus,
					ErrorMessage: tt.preflightErrorMessage,
				},
			}
			activity.nodeAgentClient = nodeAgent

			err := activity.nodePreflight(ctx, node.ID)
			assert.Equal(t, tt.wantVersionCalls, nodeAgent.AgentVersionCalls)
			assert.Equal(t, tt.wantPreflightCalls, nodeAgent.PreflightCalls)
			if nodeAgent.PreflightCalls > 0 {
				version := tt.agentVersion
				if tt.wantVersionCalls > 0 {
					version = tt.probedAgentVersion
				}
				assert.Equal(t, version, nodeAgent.LastPreflightNode.AgentVersion)
			}
			storedNode, getErr := activity.db.GetNode(ctx, node.ID)
			require.NoError(t, getErr)
			assert.Equal(t, tt.agentVersion, storedNode.AgentVersion)
			if tt.probeErr != nil {
				require.ErrorIs(t, err, tt.probeErr)
			}
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				if tt.wantBenignError {
					var applicationErr *temporal.ApplicationError
					require.ErrorAs(t, err, &applicationErr)
					assert.Equal(t, nodePreflightNotReadyErrorType, applicationErr.Type())
					assert.Equal(t, temporal.ApplicationErrorCategoryBenign, applicationErr.Category())
					assert.False(t, applicationErr.NonRetryable())
				}
				if tt.wantOrdinaryError {
					var applicationErr *temporal.ApplicationError
					assert.NotErrorAs(t, err, &applicationErr)
				}
				return
			}
			require.NoError(t, err)

			if tt.assertPreflightRequest != nil {
				require.NotNil(t, nodeAgent.LastPreflightReq)
				tt.assertPreflightRequest(t, nodeAgent.LastPreflightReq)
			}
		})
	}
}

func TestUnit_NodePreflightVersionBecomesAvailable(t *testing.T) {
	ctx := t.Context()
	activity, node := setupNodePreflightRequestTest(t, ctx, uuid.New(), nil, nil)
	activity.cfg.NodeHealthCheckDisabled = true
	nodeAgent := &agenttest.FakeClient{
		PreflightResp: &nvkeproto.ExecuteNodePreflightResponse{Status: "ready"},
	}
	activity.nodeAgentClient = nodeAgent
	err := activity.nodePreflight(ctx, node.ID)
	var pending *temporal.ApplicationError
	require.ErrorAs(t, err, &pending)
	require.Equal(t, temporal.ApplicationErrorCategoryBenign, pending.Category())
	require.False(t, pending.NonRetryable())
	require.Zero(t, nodeAgent.PreflightCalls)

	nodeAgent.AgentVersion = "0.1.7"
	require.NoError(t, activity.nodePreflight(ctx, node.ID))
	require.Equal(t, 1, nodeAgent.PreflightCalls)
	require.Equal(t, "0.1.7", nodeAgent.LastPreflightNode.AgentVersion)
	require.Equal(t, 2, nodeAgent.AgentVersionCalls)
	storedNode, err := activity.db.GetNode(ctx, node.ID)
	require.NoError(t, err)
	require.Empty(t, storedNode.AgentVersion)
}

func setupNodePreflightRequestTest(
	t *testing.T,
	ctx context.Context,
	siteID uuid.UUID,
	capiInfraCluster json.RawMessage,
	capiInfraMachineTemplate json.RawMessage,
) (*Activities, db.Node) {
	t.Helper()

	bunDB := testutil.SetupDB(ctx, t,
		(*db.Cluster)(nil),
		(*db.NodePool)(nil),
		(*db.NodeSet)(nil),
		(*db.Node)(nil),
	)
	database := db.NewDB(bunDB)

	cluster := db.Cluster{
		ID:                uuid.New(),
		Name:              "test-cluster",
		KubernetesVersion: "1.34.0",
	}
	// The cases without a site are the CAPI ones; the provider is explicit on the
	// row now rather than inferred from whether a site is set.
	if siteID == uuid.Nil {
		cluster.SetCAPIProvider(db.CAPIClusterProvider{InfraCluster: capiInfraCluster})
	} else {
		cluster.SetNicoProvider(db.NicoClusterProvider{
			OrgID:  "test-org",
			SiteID: siteID,
			VpcID:  uuid.New(),
		})
	}
	_, err := database.CreateCluster(ctx, &cluster)
	require.NoError(t, err)

	nodePoolID := uuid.New()
	nodeSetID := uuid.New()
	nicoPool := db.NicoNodePoolProvider{
		SubnetIDs:             []uuid.UUID{},
		VpcPrefixIDs:          []uuid.UUID{},
		InstanceTypeID:        uuid.New(),
		SSHKeyGroupIDs:        []uuid.UUID{},
		Release:               "os",
		ConfigProfile:         "config",
		DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
	}
	capiPool := db.CAPINodePoolProvider{
		InfraMachineTemplate:   capiInfraMachineTemplate,
		BootstrapFiles:         []db.CloudInitFile{},
		PreKubeadmCommands:     []string{},
		SpireAttestationMethod: db.SpireAttestationMethodTPM,
	}

	nodePool := db.NodePool{
		ID:                   nodePoolID,
		ClusterID:            cluster.ID,
		Name:                 "worker",
		NodeNamePrefix:       "w",
		SystemReservedMemory: "1Gi",
		NodeLabels:           map[string]string{},
		KubernetesVersion:    "1.34.0",
		CurrentNodeSetID:     pointerTo(nodeSetID),
	}
	nodeSet := db.NodeSet{
		ID:                   nodeSetID,
		NodePoolID:           nodePoolID,
		DesiredCount:         1,
		KubernetesVersion:    "1.34.0",
		SystemReservedMemory: "1Gi",
	}
	if siteID == uuid.Nil {
		nodePool.SetCapi(capiPool)
		nodeSet.SetCapi(capiPool)
	} else {
		nodePool.SetNico(nicoPool)
		nodeSet.SetNico(nicoPool)
	}

	_, err = database.CreateNodePool(ctx, &nodePool)
	require.NoError(t, err)
	_, err = database.CreateNodeSet(ctx, &nodeSet)
	require.NoError(t, err)

	node := db.Node{
		ID:                     uuid.New(),
		SpireAttestationMethod: db.SpireAttestationMethodTPM,
		NodePoolID:             nodePoolID,
		NodeSetID:              nodeSetID,
		ClusterID:              cluster.ID,
		Name:                   "w-1",
		IP:                     "10.0.0.10",
	}
	// Only a nico node carries a nico payload; a CAPI node is left without one
	// rather than given an empty shell.
	if siteID != uuid.Nil {
		poolNico, err := nodePool.NicoProvider()
		require.NoError(t, err)
		node.SetNico(db.NicoNodeProvider{InstanceTypeID: poolNico.InstanceTypeID})
	}
	_, err = database.CreateNode(ctx, &node)
	require.NoError(t, err)

	return &Activities{db: database}, node
}
