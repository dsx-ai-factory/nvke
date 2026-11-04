// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestListReleasesListsClusterReleasesAndSkipsDeprecated(t *testing.T) {
	t.Parallel()

	svc := NewReleaseService(slog.Default(), releaseClient(t,
		testClusterRelease("nvke-b", false),
		testClusterRelease("nvke-a", false),
		testClusterRelease("nvke-old", true),
		testNodePoolRelease("nvke-worker-only", false),
	))

	resp, err := svc.ListReleases(t.Context(), api.ListReleasesRequestObject{})
	require.NoError(t, err)
	ok, isOK := resp.(api.ListReleases200JSONResponse)
	require.True(t, isOK)
	assert.Equal(t, []api.Release{{Name: "nvke-a"}, {Name: "nvke-b"}}, ok.Releases)
}

func TestListReleasesNoReleasesReturnsEmptyList(t *testing.T) {
	t.Parallel()

	resp, err := NewReleaseService(slog.Default(), releaseClient(t)).ListReleases(t.Context(), api.ListReleasesRequestObject{})
	require.NoError(t, err)
	ok, isOK := resp.(api.ListReleases200JSONResponse)
	require.True(t, isOK)
	assert.NotNil(t, ok.Releases)
	assert.Empty(t, ok.Releases)
}

func TestListReleasesFailsWithoutReaderOrOnListError(t *testing.T) {
	t.Parallel()

	_, err := NewReleaseService(slog.Default(), nil).ListReleases(t.Context(), api.ListReleasesRequestObject{})
	require.ErrorContains(t, err, "release reader is not configured")

	_, err = NewReleaseService(slog.Default(), failingListClient(t, errors.New("boom"))).ListReleases(t.Context(), api.ListReleasesRequestObject{})
	require.ErrorContains(t, err, "failed to list ClusterReleases: boom")
}

func failingListClient(t *testing.T, err error) ctrlclient.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(releaseScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectList, ...ctrlclient.ListOption) error {
			return err
		},
	}).Build()
}

func selectionReleaseClient(t *testing.T) ctrlclient.Client {
	t.Helper()
	return releaseClient(t,
		testClusterRelease("cp-active", false),
		testClusterRelease("cp-deprecated", true),
		testNodePoolRelease("worker-active", false),
		testNodePoolRelease("worker-deprecated", true),
	)
}

func failingGetClient(t *testing.T, err error) ctrlclient.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(releaseScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectKey, ctrlclient.Object, ...ctrlclient.GetOption) error {
			return err
		},
	}).Build()
}

func TestValidateReleaseSelectable(t *testing.T) {
	reader := selectionReleaseClient(t)
	failing := failingGetClient(t, errors.New("api server unavailable"))

	tests := []struct {
		name           string
		validate       func(context.Context, ctrlclient.Reader, string) error
		reader         ctrlclient.Reader
		release        string
		wantBadRequest string
		wantErr        string
	}{
		{name: "active ClusterRelease", validate: validateClusterReleaseSelectable, reader: reader, release: "cp-active"},
		{
			name:           "deprecated ClusterRelease",
			validate:       validateClusterReleaseSelectable,
			reader:         reader,
			release:        "cp-deprecated",
			wantBadRequest: `release "cp-deprecated" is deprecated and cannot be selected`,
		},
		{name: "active NodePoolRelease", validate: validateNodePoolReleaseSelectable, reader: reader, release: "worker-active"},
		{
			name:           "deprecated NodePoolRelease",
			validate:       validateNodePoolReleaseSelectable,
			reader:         reader,
			release:        "worker-deprecated",
			wantBadRequest: `release "worker-deprecated" is deprecated and cannot be selected`,
		},
		{
			name:           "release that is not installed",
			validate:       validateNodePoolReleaseSelectable,
			reader:         reader,
			release:        "missing",
			wantBadRequest: `release "missing" is not installed`,
		},
		{
			name:           "a ClusterRelease name is not a NodePoolRelease",
			validate:       validateNodePoolReleaseSelectable,
			reader:         reader,
			release:        "cp-active",
			wantBadRequest: `release "cp-active" is not installed`,
		},
		{name: "empty release is left to field validation", validate: validateClusterReleaseSelectable, reader: reader, release: ""},
		{name: "nil reader skips the check", validate: validateClusterReleaseSelectable, reader: nil, release: "cp-deprecated"},
		{
			name:     "read failure is not a bad request",
			validate: validateClusterReleaseSelectable,
			reader:   failing,
			release:  "cp-active",
			wantErr:  `checking release "cp-active"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.validate(t.Context(), tt.reader, tt.release)
			var badRequest *badRequestError
			switch {
			case tt.wantBadRequest != "":
				require.ErrorAs(t, err, &badRequest)
				require.EqualError(t, err, tt.wantBadRequest)
			case tt.wantErr != "":
				require.ErrorContains(t, err, tt.wantErr)
				require.NotErrorAs(t, err, &badRequest, "a read failure must not be reported as a bad request")
			default:
				require.NoError(t, err)
			}
		})
	}
}

func TestCreateNodePool_RejectsDeprecatedRelease(t *testing.T) {
	tests := []struct {
		name    string
		reader  ctrlclient.Reader
		want400 string
		wantErr string
	}{
		{
			name:    "deprecated release",
			reader:  selectionReleaseClient(t),
			want400: `release "worker-deprecated" is deprecated and cannot be selected`,
		},
		{
			name:    "release read failure",
			reader:  failingGetClient(t, errors.New("api server unavailable")),
			wantErr: "api server unavailable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := forgeCluster()
			cluster.ID = uuid.New()
			database := &testutil.MockDB{}
			database.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
			service := NewNodePoolService(slog.Default(), database, config.TeleportConfig{}, nil)
			service.releaseReader = tt.reader

			resp, err := service.CreateNodePool(t.Context(), api.CreateNodePoolRequestObject{Body: &api.CreateNodePoolRequest{
				ClusterID:      cluster.ID,
				Name:           "workers",
				NodeNamePrefix: "w",
				Nico: &api.NicoNodePoolProvider{
					SubnetIDs:      []uuid.UUID{uuid.New()},
					InstanceTypeID: uuid.New(),
					Release:        "worker-deprecated",
				},
				DesiredNodeCount: 1,
			}})

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
				badRequest, ok := resp.(api.CreateNodePool400JSONResponse)
				require.True(t, ok, "expected CreateNodePool400JSONResponse, got %T", resp)
				require.Equal(t, tt.want400, badRequest.Message)
			}
			database.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
		})
	}
}

func TestCreateCluster_RejectsDeprecatedRelease(t *testing.T) {
	tests := []struct {
		name          string
		cpRelease     string
		workerRelease string
		want400       string
	}{
		{name: "active releases", cpRelease: "cp-active", workerRelease: "worker-active"},
		{
			name:          "deprecated control-plane release",
			cpRelease:     "cp-deprecated",
			workerRelease: "worker-active",
			want400:       `release "cp-deprecated" is deprecated and cannot be selected`,
		},
		{
			name:          "deprecated worker release",
			cpRelease:     "cp-active",
			workerRelease: "worker-deprecated",
			want400:       `release "worker-deprecated" is deprecated and cannot be selected`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
			nodePoolService := NewNodePoolService(slog.Default(), database, config.TeleportConfig{}, nil)
			service := NewClusterService(slog.Default(), database, nodePoolService, config.TeleportConfig{}, false, nil, true, "")
			service.releaseReader = selectionReleaseClient(t)

			req := validForgeCreateClusterRequest()
			req.ControlPlaneConfig = validForgeControlPlaneConfig()
			req.ControlPlaneConfig.DesiredCount = 1
			req.ControlPlaneConfig.Nico.SubnetIDs = []uuid.UUID{uuid.New()}
			req.ControlPlaneConfig.Nico.Release = tt.cpRelease
			worker := validForgeNodePoolCreateConfig("workers")
			worker.Nico.Release = tt.workerRelease
			req.NodePools = &[]api.NodePoolCreateConfig{worker}

			if tt.want400 == "" {
				_, err := service.preflightCheckCreateClusterRequest(t.Context(), req)
				require.NoError(t, err)
				return
			}
			resp, err := service.CreateCluster(t.Context(), api.CreateClusterRequestObject{Body: req})
			require.NoError(t, err)
			badRequest, ok := resp.(api.CreateCluster400JSONResponse)
			require.True(t, ok, "expected CreateCluster400JSONResponse, got %T", resp)
			require.Equal(t, tt.want400, badRequest.Message)
			database.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
		})
	}
}

func TestUpdateNodePool_RejectsSwitchToDeprecatedRelease(t *testing.T) {
	tests := []struct {
		name    string
		current string
		release string
		want400 string
	}{
		{name: "switch to an active release", current: "worker-deprecated", release: "worker-active"},
		{name: "resend the deprecated release the pool already has", current: "worker-deprecated", release: "worker-deprecated"},
		{
			name:    "switch to a deprecated release",
			current: "worker-active",
			release: "worker-deprecated",
			want400: `release "worker-deprecated" is deprecated and cannot be selected`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := forgeCluster()
			cluster.ID = uuid.New()
			nodePool := db.NodePool{ID: uuid.New(), ClusterID: cluster.ID, DesiredCount: 1}
			nodePool.SetNico(db.NicoNodePoolProvider{
				SubnetIDs:             []uuid.UUID{uuid.New()},
				Release:               tt.current,
				DiskSelectionStrategy: db.DiskSelectionStrategy(api.DiskSelectionStrategyCreateSmallest),
			})
			tx := &testutil.MockTx{}
			tx.On("GetNodePool", mock.Anything, nodePool.ID).Return(nodePool, nil)
			tx.On("GetClusterForUpdate", mock.Anything, cluster.ID).Return(cluster, nil)
			tx.On("GetNodePoolForUpdate", mock.Anything, nodePool.ID).Return(nodePool, nil)
			tx.On("UpdateNodePool", mock.Anything, mock.Anything).Maybe().Return(nodePool, nil)
			database := &testutil.MockDB{MockTx: tx}
			database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
			service := NewNodePoolService(slog.Default(), database, config.TeleportConfig{}, nil)
			service.releaseReader = selectionReleaseClient(t)

			resp, err := service.UpdateNodePool(t.Context(), api.UpdateNodePoolRequestObject{
				NodePoolID: nodePool.ID,
				Body:       &api.UpdateNodePoolRequest{Nico: &api.NicoNodePoolProviderUpdate{Release: ptrString(tt.release)}},
			})

			require.NoError(t, err)
			if tt.want400 == "" {
				require.IsType(t, api.UpdateNodePool200JSONResponse{}, resp)
				return
			}
			badRequest, ok := resp.(api.UpdateNodePool400JSONResponse)
			require.True(t, ok, "expected UpdateNodePool400JSONResponse, got %T", resp)
			require.Equal(t, tt.want400, badRequest.Message)
			tx.AssertNotCalled(t, "UpdateNodePool", mock.Anything, mock.Anything)
		})
	}
}

func TestApplyClusterLevelControlPlaneSpecUpdate_RejectsSwitchToDeprecatedRelease(t *testing.T) {
	tests := []struct {
		name    string
		current string
		release *string
		want400 string
	}{
		{name: "release not in the patch", current: "cp-deprecated"},
		{name: "switch to an active release", current: "cp-deprecated", release: ptrString("cp-active")},
		{name: "resend the deprecated release the control plane already has", current: "cp-deprecated", release: ptrString("cp-deprecated")},
		{
			name:    "switch to a deprecated release",
			current: "cp-active",
			release: ptrString("cp-deprecated"),
			want400: `release "cp-deprecated" is deprecated and cannot be selected`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := clusterService{releaseReader: selectionReleaseClient(t)}
			cluster := forgeCluster()
			var spec db.ControlPlaneConfigSpec
			spec.SetNico(db.NicoControlPlaneProvider{Release: tt.current})

			err := service.applyClusterLevelControlPlaneSpecUpdate(t.Context(), &cluster, &api.UpdateClusterControlPlaneConfig{
				Nico: &api.NicoControlPlaneProviderUpdate{Release: tt.release},
			}, &spec)

			if tt.want400 == "" {
				require.NoError(t, err)
				return
			}
			var badRequest *badRequestError
			require.ErrorAs(t, err, &badRequest)
			require.EqualError(t, err, tt.want400)
			require.Equal(t, tt.current, spec.Nico.Release, "a rejected patch must not change the stored release")
		})
	}
}

func TestUpdateCluster_RejectsSwitchToDeprecatedControlPlaneRelease(t *testing.T) {
	cluster := forgeCluster()
	cluster.ID = uuid.New()
	cluster.ControlPlaneConfig.SetNico(db.NicoControlPlaneProvider{
		InstanceTypePreference: []uuid.UUID{uuid.New()},
		SubnetIDs:              []uuid.UUID{uuid.New()},
		Release:                "cp-active",
	})
	database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	database.MockTx.On("GetClusterForUpdate", mock.Anything, cluster.ID).Return(cluster, nil)
	service := NewClusterService(slog.Default(), database, nil, config.TeleportConfig{}, false, nil, true, "")
	service.releaseReader = selectionReleaseClient(t)

	resp, err := service.UpdateCluster(t.Context(), api.UpdateClusterRequestObject{
		ClusterID: cluster.ID,
		Body: &api.UpdateClusterRequest{ControlPlaneConfig: &api.UpdateClusterControlPlaneConfig{
			Nico: &api.NicoControlPlaneProviderUpdate{Release: ptrString("cp-deprecated")},
		}},
	})

	require.NoError(t, err)
	badRequest, ok := resp.(api.UpdateCluster400JSONResponse)
	require.True(t, ok, "expected UpdateCluster400JSONResponse, got %T", resp)
	require.Equal(t, `release "cp-deprecated" is deprecated and cannot be selected`, badRequest.Message)
	database.MockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
}
