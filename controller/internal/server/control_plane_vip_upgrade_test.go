// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/server"
)

func TestClusterService_UpdateCluster_UpgradeWithoutVIP(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(fmt.Sprintf("override=%t", allow), func(t *testing.T) {
			clusterID := uuid.New()
			oldConfigID := uuid.New()
			newConfigID := uuid.New()
			currentVersion := "1.34.2"
			targetVersion := "1.35.3"
			enableRecycling := true
			currentSpec := db.ControlPlaneConfigSpec{
				KubernetesVersion: currentVersion,
				Capi:              &db.CAPIControlPlaneProvider{},
			}
			cluster := db.Cluster{
				ID:                clusterID,
				Name:              "no-vip",
				KubernetesVersion: currentVersion,
				ProviderType:      db.ProviderTypeCAPI,
				Capi:              &db.CAPIClusterProvider{},
				ControlPlaneConfig: db.ControlPlaneConfig{
					ConfigurationID:        oldConfigID,
					DesiredCount:           1,
					MaxSurge:               1,
					ControlPlaneConfigSpec: currentSpec,
				},
			}

			mockDB := &mockDB{MockTx: &mockTx{}}
			mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
			mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil).Once()
			controlPlaneFalse := false
			mockDB.MockTx.On("ListNodePools", mock.Anything, db.NodePoolFilters{
				ClusterID:    &clusterID,
				ControlPlane: &controlPlaneFalse,
			}).Return([]db.NodePool{}, nil).Maybe()
			mockDB.MockTx.On("GetControlPlaneConfiguration", mock.Anything, oldConfigID).Return(db.ControlPlaneConfiguration{
				ID:        oldConfigID,
				ClusterID: clusterID,
				Config:    currentSpec,
			}, nil).Maybe()
			mockDB.MockTx.On("CreateControlPlaneConfiguration", mock.Anything, mock.Anything).Return(db.ControlPlaneConfiguration{
				ID:        newConfigID,
				ClusterID: clusterID,
				Config: db.ControlPlaneConfigSpec{
					KubernetesVersion: targetVersion,
					Capi:              &db.CAPIControlPlaneProvider{},
				},
			}, nil).Maybe()
			mockDB.MockTx.On("UpdateCluster", mock.Anything, mock.Anything).Return(db.Cluster{
				ID:                clusterID,
				Name:              cluster.Name,
				KubernetesVersion: targetVersion,
				ProviderType:      db.ProviderTypeCAPI,
				Capi:              &db.CAPIClusterProvider{},
				ControlPlaneConfig: db.ControlPlaneConfig{
					ConfigurationID: newConfigID,
					DesiredCount:    1,
					MaxSurge:        1,
				},
			}, nil).Maybe()
			mockDB.MockTx.On("GetControlPlaneConfiguration", mock.Anything, newConfigID).Return(db.ControlPlaneConfiguration{
				ID:        newConfigID,
				ClusterID: clusterID,
				Config: db.ControlPlaneConfigSpec{
					KubernetesVersion: targetVersion,
					Capi:              &db.CAPIControlPlaneProvider{},
				},
			}, nil).Maybe()

			svc := server.NewServerImpl(config.Config{AllowControlPlaneWithoutVIP: allow}, discardLogger(), mockDB, nil, nil, nil, nil)
			resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
				ClusterID: clusterID,
				Body: &api.UpdateClusterRequest{
					ControlPlaneKubernetesVersion: &targetVersion,
					EnableCPNodeRecycling:         &enableRecycling,
				},
			})
			require.NoError(t, err)

			if allow {
				require.IsType(t, api.UpdateCluster200JSONResponse{}, resp)
				mockDB.MockTx.AssertCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
			} else {
				badRequest, ok := resp.(api.UpdateCluster400JSONResponse)
				require.True(t, ok, "expected UpdateCluster400JSONResponse, got %T", resp)
				assert.Contains(t, badRequest.Message, "controlPlaneVIP")
				mockDB.MockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
			}
		})
	}
}
