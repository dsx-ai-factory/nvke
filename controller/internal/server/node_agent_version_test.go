// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestCanonicalNodeAgentVersion(t *testing.T) {
	for _, test := range []struct {
		input, want string
		valid       bool
	}{
		{"", "", true},
		{"1.2.3", "v1.2.3", true},
		{"v1.2.3", "v1.2.3", true},
		{"latest", "", false},
		{"1.2", "", false},
		{"v01.2.3", "", false},
		{"v0.3.0-rc.2", "v0.3.0-rc.2", true},
		{"0.3.0-rc.2", "v0.3.0-rc.2", true},
		{"v1.2.3-0", "v1.2.3-0", true},
		{"v1.2.3-alpha-beta.1", "v1.2.3-alpha-beta.1", true},
		{"v1.2.3-rc.01", "", false},
		{"v1.2.3-rc..1", "", false},
		{"v1.2.3-", "", false},
		{"v1.2.3-rc.1+build", "", false},
		{"v1.2.3-rc/1", "", false},
		{"v1.2.3+build", "", false},
		{" v1.2.3", "", false},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := canonicalNodeAgentVersion(test.input)
			if test.valid {
				require.NoError(t, err)
				require.Equal(t, test.want, got)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestAgentVersionChangeDoesNotReplaceNodes(t *testing.T) {
	pinned := db.ControlPlaneConfigSpec{KubernetesVersion: "1.34.2", NodeAgentConfig: db.NodeAgentConfig{Version: "v1.2.3"}}
	desired := pinned
	desired.NodeAgentConfig.Version = "v1.2.2"
	require.False(t, db.IsControlPlaneRolloutRequired(pinned, desired))
	desired.NodeAgentConfig.Version = ""
	require.False(t, db.IsControlPlaneRolloutRequired(pinned, desired))
	desired.KubernetesVersion = "1.34.3"
	require.True(t, db.IsControlPlaneRolloutRequired(pinned, desired))
}

func TestNodeAgentVersionPatchPreservesOmittedAndClearsEmpty(t *testing.T) {
	service := clusterService{nodeAgentUpdateMethod: "apt"}
	ctx := context.Background()
	spec := db.ControlPlaneConfigSpec{NodeAgentConfig: db.NodeAgentConfig{Version: "v1.2.3"}}
	cluster := db.Cluster{}
	require.NoError(t, service.applyClusterLevelControlPlaneSpecUpdate(ctx, &cluster, &api.UpdateClusterControlPlaneConfig{}, &spec))
	require.Equal(t, "v1.2.3", spec.NodeAgentConfig.Version)
	require.NoError(t, service.applyClusterLevelControlPlaneSpecUpdate(ctx, &cluster, &api.UpdateClusterControlPlaneConfig{NodeAgentConfig: &api.NodeAgentConfig{}}, &spec))
	require.Equal(t, "v1.2.3", spec.NodeAgentConfig.Version)
	downgrade := "0.3.0-rc.2"
	require.NoError(t, service.applyClusterLevelControlPlaneSpecUpdate(ctx, &cluster, &api.UpdateClusterControlPlaneConfig{NodeAgentConfig: &api.NodeAgentConfig{Version: &downgrade}}, &spec))
	require.Equal(t, "v0.3.0-rc.2", spec.NodeAgentConfig.Version)
	empty := ""
	require.NoError(t, service.applyClusterLevelControlPlaneSpecUpdate(ctx, &cluster, &api.UpdateClusterControlPlaneConfig{NodeAgentConfig: &api.NodeAgentConfig{Version: &empty}}, &spec))
	require.Empty(t, spec.NodeAgentConfig.Version)
}

func TestClusterDeletionLeavesAcceptedAgentUpgradeAlone(t *testing.T) {
	cluster := db.Cluster{ID: uuid.New(), NodeAgentUpgrade: &db.NodeAgentUpgrade{NodeID: uuid.New(), Phase: "Running"}}
	database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	database.MockTx.On("GetClusterForUpdate", mock.Anything, cluster.ID).Return(cluster, nil)
	database.MockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(updated *db.Cluster) bool {
		return updated.NodeAgentUpgrade == cluster.NodeAgentUpgrade && updated.Conditions.Deleting.Status == db.ConditionStatusTrue
	})).Return(cluster, nil).Once()
	database.MockTx.On("CreateWorkflowOutboxEntry", mock.Anything, "TeardownCluster", mock.Anything, map[string]interface{}{clusterIDConst: cluster.ID}).Return(db.WorkflowOutbox{}, nil).Once()
	service := clusterService{db: database}
	response, err := service.DeleteCluster(context.Background(), api.DeleteClusterRequestObject{ClusterID: cluster.ID})
	require.NoError(t, err)
	require.IsType(t, api.DeleteCluster202Response{}, response)
	database.MockTx.AssertExpectations(t)
}

func TestAgentUpgradeMaintenanceReturnsConflict(t *testing.T) {
	for _, maintenance := range []string{"OIDC", "etcd encryption"} {
		t.Run(maintenance, func(t *testing.T) {
			cluster := db.Cluster{ID: uuid.New(), NodeAgentUpgrade: &db.NodeAgentUpgrade{NodeID: uuid.New(), Phase: "Running"}}
			database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
			database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
			database.MockTx.On("GetClusterForUpdate", mock.Anything, cluster.ID).Return(cluster, nil)
			body := &api.UpdateClusterRequest{}
			enabled := true
			if maintenance == "OIDC" {
				body.EnableOidc = &enabled
			} else {
				body.EnableEtcdEncryption = &enabled
			}
			service := clusterService{db: database}
			response, err := service.UpdateCluster(t.Context(), api.UpdateClusterRequestObject{ClusterID: cluster.ID, Body: body})
			require.NoError(t, err)
			conflict, ok := response.(api.UpdateCluster409JSONResponse)
			require.True(t, ok)
			require.Equal(t, errAgentUpgradeInProgress.Error(), conflict.Message)
			database.MockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
		})
	}
}

func TestAgentUpgradeResponseHasOnlyPendingOrRunningPhase(t *testing.T) {
	for _, test := range []struct {
		phase string
		want  api.NodeAgentUpgradeStatusPhase
	}{
		{phase: "Pending", want: "Pending"},
		{phase: "Running", want: "Running"},
	} {
		t.Run(test.phase, func(t *testing.T) {
			response := apiNodeAgentUpgradeFromDB(&db.NodeAgentUpgrade{Phase: test.phase, ErrorMessage: "last attempt failed"})
			require.Equal(t, test.want, response.Phase)
			require.Equal(t, "last attempt failed", response.ErrorMessage)
		})
	}
	require.Nil(t, apiNodeAgentUpgradeFromDB(nil))
}
