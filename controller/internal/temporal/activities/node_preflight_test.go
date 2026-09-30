// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	temporallog "go.temporal.io/sdk/log"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func TestNodePreflight(t *testing.T) {
	ctx := context.Background()
	versionErr := errors.New("dial refused")
	logger := temporallog.NewStructuredLogger(slog.New(slog.DiscardHandler))

	tests := []struct {
		name                     string
		siteID                   uuid.UUID
		capiInfraCluster         json.RawMessage
		capiInfraMachineTemplate json.RawMessage
		agentVersion             string
		agentVersionErr          error
		wantErr                  string
		wantPreflightCalls       int
		assertPreflightRequest   func(t *testing.T, req *nkeproto.ExecuteNodePreflightRequest)
	}{
		{
			name:                     "old agent skips strict preflight after agent version probe",
			siteID:                   uuid.New(),
			capiInfraCluster:         json.RawMessage(`{}`),
			capiInfraMachineTemplate: json.RawMessage(`{}`),
			agentVersion:             "0.1.22",
		},
		{
			name:                     "Forge node with current agent checks cloud-init status",
			siteID:                   uuid.New(),
			capiInfraCluster:         json.RawMessage(`{}`),
			capiInfraMachineTemplate: json.RawMessage(`{}`),
			agentVersion:             "0.1.24",
			wantPreflightCalls:       1,
			assertPreflightRequest: func(t *testing.T, req *nkeproto.ExecuteNodePreflightRequest) {
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
			assertPreflightRequest: func(t *testing.T, req *nkeproto.ExecuteNodePreflightRequest) {
				t.Helper()
				assert.Nil(t, req.GetCheckCloudInitStatus())
				checks := req.GetChecks()
				require.Len(t, checks, 1)
				assert.Equal(t, "cluster-api-bootstrap-success", checks[0].GetName())
				assert.Equal(t, capdBootstrapSuccessPath, checks[0].GetFileExists().GetPath())
			},
		},
		{
			name:            "agent version probe failure reports node agent not ready",
			siteID:          uuid.New(),
			agentVersionErr: versionErr,
			wantErr:         "node agent is not ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			activity, node := setupNodePreflightRequestTest(t, ctx, tt.siteID, tt.capiInfraCluster, tt.capiInfraMachineTemplate)
			nodeAgent := &agenttest.FakeClient{
				AgentVersion:    tt.agentVersion,
				AgentVersionErr: tt.agentVersionErr,
				PreflightResp: &nkeproto.ExecuteNodePreflightResponse{
					Status: nodePreflightStatusReady,
				},
			}
			activity.nodeAgentClient = nodeAgent

			err := activity.nodePreflight(ctx, node.ID, logger)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)

			assert.Equal(t, tt.wantPreflightCalls, nodeAgent.PreflightCalls)
			if tt.assertPreflightRequest != nil {
				require.NotNil(t, nodeAgent.LastPreflightReq)
				tt.assertPreflightRequest(t, nodeAgent.LastPreflightReq)
			}
		})
	}
}

func TestNodeAgentVersionAtLeast(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    bool
	}{
		{name: "minimum release", version: "v0.1.26", want: true},
		{name: "git describe after minimum", version: "v0.1.26-2-g1f5274f-dirty", want: true},
		{name: "dirty minimum tag", version: "v0.1.26-dirty", want: true},
		{name: "git describe after older release", version: "v0.1.25-9-g1f5274f", want: false},
		{name: "older release", version: "0.1.25", want: false},
		{name: "minimum prerelease", version: "v0.1.26-rc.1", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, nodeAgentVersionAtLeast(tt.version, "0.1.26", false))
		})
	}
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
		OSProfile:             "os",
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
		NodeLabels:           []string{},
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
