// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func expectClusterCreateWithCNI(tx *mockTx, clusterID uuid.UUID, storedPlugin *db.CniPlugin, storedVersion *string) {
	tx.On("CreateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
		*storedPlugin, *storedVersion = c.CniPlugin, c.CniVersion
		return true
	})).Return(forgeDBCluster(db.Cluster{ID: clusterID, Name: "test-cluster"}), nil)
	tx.On("CreateWorkflowOutboxEntry", mock.Anything, "ProvisionCluster", mock.Anything, mock.Anything).Return(db.WorkflowOutbox{}, nil)
	expectControlPlaneConfigCreate(tx, clusterID)
}

func TestClusterService_CreateClusterCNIPlugin(t *testing.T) {
	ovnVersion := "1.4.0"

	tests := []struct {
		name              string
		plugin            api.CreateClusterRequestCniPlugin
		cniVersion        *string
		allowLegacyCalico bool
		wantStatus        int
		wantMessage       string
		wantPlugin        db.CniPlugin
		wantVersion       string
	}{
		{
			name:        "an omitted plugin is ovn-kubernetes with its default version",
			wantStatus:  http.StatusCreated,
			wantPlugin:  db.CNIOvnKubernetes,
			wantVersion: db.DefaultOvnKubernetesCNIVersion,
		},
		{
			name:        "an explicit ovn-kubernetes version is stored",
			plugin:      api.CreateClusterRequestCniPluginOvnKubernetes,
			cniVersion:  &ovnVersion,
			wantStatus:  http.StatusCreated,
			wantPlugin:  db.CNIOvnKubernetes,
			wantVersion: ovnVersion,
		},
		{
			name:              "calico is stored with its default version when legacy creation is enabled",
			plugin:            api.CreateClusterRequestCniPluginCalico,
			allowLegacyCalico: true,
			wantStatus:        http.StatusCreated,
			wantPlugin:        db.CNICalico,
			wantVersion:       db.DefaultCalicoCNIVersion,
		},
		{
			name:        "calico is refused when legacy creation is disabled",
			plugin:      api.CreateClusterRequestCniPluginCalico,
			wantStatus:  http.StatusBadRequest,
			wantMessage: "calico cluster creation is disabled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clusterID := uuid.New()
			mockDB := &mockDB{MockTx: &mockTx{}}
			service := server.NewClusterService(
				discardLogger(), mockDB, server.NewNodePoolService(discardLogger(), mockDB, config.TeleportConfig{}, nil),
				config.TeleportConfig{}, false, nil, tt.allowLegacyCalico, "",
			)

			var storedPlugin db.CniPlugin
			var storedVersion string
			if tt.wantStatus == http.StatusCreated {
				expectClusterCreateWithCNI(mockDB.MockTx, clusterID, &storedPlugin, &storedVersion)
				mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
			}

			request := baseNicoClusterCreateRequest()
			request.CniPlugin = tt.plugin
			request.CniVersion = tt.cniVersion
			resp, err := service.CreateCluster(context.Background(), api.CreateClusterRequestObject{Body: request})
			require.NoError(t, err)

			recorder := httptest.NewRecorder()
			require.NoError(t, resp.VisitCreateClusterResponse(recorder))
			require.Equal(t, tt.wantStatus, recorder.Result().StatusCode, recorder.Body.String())

			if tt.wantStatus == http.StatusCreated {
				assert.Equal(t, tt.wantPlugin, storedPlugin)
				assert.Equal(t, tt.wantVersion, storedVersion)
				return
			}
			var body struct {
				Message string `json:"message"`
			}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
			assert.Contains(t, body.Message, tt.wantMessage)
			mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
		})
	}
}
