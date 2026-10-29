// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestLaunchLayerSecurityGroups_CreateReadPersistence(t *testing.T) {
	for name, attachments := range map[string]string{
		"omitted":          `{}`,
		"empty":            `{"securityGroups":{"primaryNetworkInterface":[],"gpuNetworkInterface":[]}}`,
		"managed":          `{"securityGroupRefs":{"primaryNetworkInterface":["managed"]}}`,
		"external":         `{"securityGroups":{"primaryNetworkInterface":["external-primary"],"gpuNetworkInterface":["external-gpu"]}}`,
		"mixed interfaces": `{"securityGroupRefs":{"primaryNetworkInterface":["managed"]},"securityGroups":{"gpuNetworkInterface":["external-gpu"]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			bunDB := testutil.SetupDB(ctx, t, (*db.NodePool)(nil), (*db.NodeSet)(nil), (*db.ControlPlaneConfiguration)(nil))
			queries := db.NewDB(bunDB)
			var input api.LaunchLayerNodePoolProvider
			require.NoError(t, json.Unmarshal([]byte(attachments), &input))
			input.InstanceType = "bare-metal"
			pool, err := dbNodePoolFromAPI(&api.CreateNodePoolRequest{
				Name: "workers", ClusterID: uuid.New(), NodeNamePrefix: "worker", DesiredNodeCount: 2, LaunchLayer: &input,
			}, "1.33.1")
			require.NoError(t, err)
			_, err = queries.CreateNodePool(ctx, &pool)
			require.NoError(t, err)
			loaded, err := queries.GetNodePool(ctx, pool.ID)
			require.NoError(t, err)
			require.Equal(t, pool.LaunchLayer, loaded.LaunchLayer)
			poolOutput := apiNodePoolFromDB(&loaded).LaunchLayer
			require.NotNil(t, poolOutput)
			require.Equal(t, input.SecurityGroupRefs, poolOutput.SecurityGroupRefs)
			if name == "empty" {
				input.SecurityGroups = nil
			}
			require.Equal(t, input.SecurityGroups, poolOutput.SecurityGroups)

			nodeSet := db.NodeSet{ID: uuid.New(), NodePoolID: pool.ID}
			nodeSet.SetLaunchLayer(*loaded.LaunchLayer)
			_, err = queries.CreateNodeSet(ctx, &nodeSet)
			require.NoError(t, err)
			loadedSet, err := queries.GetNodeSet(ctx, nodeSet.ID)
			require.NoError(t, err)
			setOutput := apiLaunchLayerNodeSetProviderFromDB(loadedSet.LaunchLayer)
			require.Equal(t, input.SecurityGroups, setOutput.SecurityGroups)
			require.Equal(t, input.SecurityGroupRefs, setOutput.SecurityGroupRefs)

			var cpInput api.LaunchLayerControlPlaneProvider
			require.NoError(t, json.Unmarshal([]byte(attachments), &cpInput))
			cpInput.InstanceType = input.InstanceType
			spec := controlPlaneConfigSpecFromCreateAPI(&api.CreateClusterControlPlaneConfig{LaunchLayer: &cpInput}, "1.33.1", false, db.ProviderTypeLaunchLayer)
			snapshot, err := queries.CreateControlPlaneConfiguration(ctx, &db.ControlPlaneConfiguration{ClusterID: pool.ClusterID, Config: spec})
			require.NoError(t, err)
			loadedSnapshot, err := queries.GetControlPlaneConfiguration(ctx, snapshot.ID)
			require.NoError(t, err)
			require.Equal(t, spec.LaunchLayer, loadedSnapshot.Config.LaunchLayer)
			cfg := db.BuildControlPlaneConfig(db.Cluster{}, loadedSnapshot.Config, loadedSnapshot.CreatedAt)
			cpOutput := apiLaunchLayerControlPlaneProviderFromDB(cfg.LaunchLayer)
			require.Equal(t, input.SecurityGroups, cpOutput.SecurityGroups)
			require.Equal(t, input.SecurityGroupRefs, cpOutput.SecurityGroupRefs)
		})
	}
}

func TestLaunchLayerSecurityGroups_Admission(t *testing.T) {
	for name, tt := range map[string]struct {
		groups  *api.LaunchLayerSecurityGroups
		refs    *api.LaunchLayerSecurityGroupRefs
		wantErr string
	}{
		"external only":                {groups: &api.LaunchLayerSecurityGroups{PrimaryNetworkInterface: []string{"external-primary"}, GpuNetworkInterface: []string{"external-gpu"}}},
		"empty primary ID":             {groups: &api.LaunchLayerSecurityGroups{PrimaryNetworkInterface: []string{""}}, wantErr: "securityGroups must not contain empty IDs"},
		"empty gpu ID":                 {groups: &api.LaunchLayerSecurityGroups{GpuNetworkInterface: []string{""}}, wantErr: "securityGroups must not contain empty IDs"},
		"managed primary external gpu": {groups: &api.LaunchLayerSecurityGroups{GpuNetworkInterface: []string{"external-gpu"}}, refs: &api.LaunchLayerSecurityGroupRefs{PrimaryNetworkInterface: []string{"bm"}}},
		"external primary managed gpu": {groups: &api.LaunchLayerSecurityGroups{PrimaryNetworkInterface: []string{"external-primary"}}, refs: &api.LaunchLayerSecurityGroupRefs{GpuNetworkInterface: []string{"bm"}}},
		"empty external with managed":  {groups: &api.LaunchLayerSecurityGroups{}, refs: &api.LaunchLayerSecurityGroupRefs{PrimaryNetworkInterface: []string{"bm"}}},
		"primary conflict":             {groups: &api.LaunchLayerSecurityGroups{PrimaryNetworkInterface: []string{"external-primary"}}, refs: &api.LaunchLayerSecurityGroupRefs{PrimaryNetworkInterface: []string{"bm"}}, wantErr: "mutually exclusive on primaryNetworkInterface"},
		"gpu conflict":                 {groups: &api.LaunchLayerSecurityGroups{GpuNetworkInterface: []string{"external-gpu"}}, refs: &api.LaunchLayerSecurityGroupRefs{GpuNetworkInterface: []string{"bm"}}, wantErr: "mutually exclusive on gpuNetworkInterface"},
	} {
		t.Run(name, func(t *testing.T) {
			clusterReq := validLaunchLayerCreateClusterRequest()
			clusterReq.ControlPlaneConfig.LaunchLayer.SecurityGroups = tt.groups
			clusterReq.ControlPlaneConfig.LaunchLayer.SecurityGroupRefs = tt.refs
			if tt.refs == nil {
				clusterReq.LaunchLayer.SecurityGroups = nil
			}
			database := new(testutil.MockDB)
			svc := clusterService{db: database, providers: newProviderSupport([]string{config.InfraProviderLaunchLayer})}
			err := svc.ValidateCreateClusterRequestFields(clusterReq)
			cluster := launchLayerCluster()
			cluster.LaunchLayer.SecurityGroups = []db.LaunchLayerSecurityGroup{{Name: "bm", Type: "BARE_METAL"}}
			poolSvc := NewNodePoolService(slog.Default(), database, config.TeleportConfig{}, []string{config.InfraProviderLaunchLayer})
			poolReq := &api.CreateNodePoolRequest{
				Name: "workers", NodeNamePrefix: "worker", DesiredNodeCount: 1,
				LaunchLayer: &api.LaunchLayerNodePoolProvider{InstanceType: "bare-metal", SecurityGroupRefs: tt.refs, SecurityGroups: tt.groups},
			}
			if tt.wantErr == "" {
				require.NoError(t, err)
				require.NoError(t, poolSvc.validateCreateNodePoolRequest(cluster, poolReq))
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
			clusterResponse, err := svc.CreateCluster(t.Context(), api.CreateClusterRequestObject{Body: clusterReq})
			require.NoError(t, err)
			badClusterRequest, ok := clusterResponse.(api.CreateCluster400JSONResponse)
			require.True(t, ok)
			require.Contains(t, badClusterRequest.Message, tt.wantErr)
			database.On("GetCluster", mock.Anything, poolReq.ClusterID).Return(cluster, nil).Once()
			response, err := poolSvc.CreateNodePool(t.Context(), api.CreateNodePoolRequestObject{Body: poolReq})
			require.NoError(t, err)
			badRequest, ok := response.(api.CreateNodePool400JSONResponse)
			require.True(t, ok)
			require.Contains(t, badRequest.Message, tt.wantErr)
			database.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
			database.AssertExpectations(t)
		})
	}
}
