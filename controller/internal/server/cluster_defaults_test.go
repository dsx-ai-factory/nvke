// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/api"
	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

func ptrBool(v bool) *bool { return &v }

func ptrString(v string) *string { return &v }

func TestApplyClusterDefaults(t *testing.T) {
	svc := &clusterService{
		teleportCfg: config.TeleportConfig{DefaultClusterEnabled: true},
	}

	t.Run("fills nil with default", func(t *testing.T) {
		req := &api.CreateClusterRequest{}
		cluster := &db.Cluster{}
		require.NoError(t, svc.applyClusterDefaults(req, cluster))
		assert.True(t, cluster.TeleportEnabled)
	})

	t.Run("preserves explicit false", func(t *testing.T) {
		req := &api.CreateClusterRequest{TeleportEnabled: ptrBool(false)}
		cluster := &db.Cluster{}
		require.NoError(t, svc.applyClusterDefaults(req, cluster))
		assert.False(t, cluster.TeleportEnabled)
	})

	t.Run("preserves explicit true", func(t *testing.T) {
		svcOff := &clusterService{
			teleportCfg: config.TeleportConfig{DefaultClusterEnabled: false},
		}
		req := &api.CreateClusterRequest{TeleportEnabled: ptrBool(true)}
		cluster := &db.Cluster{TeleportEnabled: true}
		require.NoError(t, svcOff.applyClusterDefaults(req, cluster))
		assert.True(t, cluster.TeleportEnabled)
	})

	// Characterization of pre-existing behavior in applyClusterDefaults, not a
	// change: TenantAccess now selects SPIRE vs admin from the persisted
	// OIDCAuthEnabled re-read from the DB, so create-time stamping is load-bearing
	// for publish and nothing pinned it.
	t.Run("stamps OIDCAuthEnabled from create-time OIDC flag", func(t *testing.T) {
		svcOn := &clusterService{oidcAuthEnabled: true}
		clusterOn := &db.Cluster{}
		require.NoError(t, svcOn.applyClusterDefaults(&api.CreateClusterRequest{}, clusterOn))
		assert.True(t, clusterOn.OIDCAuthEnabled)

		svcOff := &clusterService{oidcAuthEnabled: false}
		clusterOff := &db.Cluster{}
		require.NoError(t, svcOff.applyClusterDefaults(&api.CreateClusterRequest{}, clusterOff))
		assert.False(t, clusterOff.OIDCAuthEnabled)
	})
}

func TestApplyNodePoolDefaults(t *testing.T) {
	svc := &nodePoolService{
		teleportCfg: config.TeleportConfig{
			DefaultCPPoolEnabled:     true,
			DefaultWorkerPoolEnabled: false,
		},
	}
	forgeCluster := dbClusterWithSiteID(uuid.New())

	t.Run("CP pool nil gets CP default", func(t *testing.T) {
		req := &api.CreateNodePoolRequest{ControlPlane: true}
		svc.applyNodePoolDefaults(req, forgeCluster)
		assert.NotNil(t, req.TeleportEnabled)
		assert.True(t, *req.TeleportEnabled)
	})

	t.Run("worker pool nil gets worker default", func(t *testing.T) {
		req := &api.CreateNodePoolRequest{ControlPlane: false}
		svc.applyNodePoolDefaults(req, forgeCluster)
		assert.NotNil(t, req.TeleportEnabled)
		assert.False(t, *req.TeleportEnabled)
	})

	t.Run("explicit value preserved for CP", func(t *testing.T) {
		req := &api.CreateNodePoolRequest{ControlPlane: true, TeleportEnabled: ptrBool(false)}
		svc.applyNodePoolDefaults(req, forgeCluster)
		assert.False(t, *req.TeleportEnabled)
	})

	t.Run("explicit value preserved for worker", func(t *testing.T) {
		req := &api.CreateNodePoolRequest{ControlPlane: false, TeleportEnabled: ptrBool(true)}
		svc.applyNodePoolDefaults(req, forgeCluster)
		assert.True(t, *req.TeleportEnabled)
	})

	// The stored pool is built from the nested payload, so the default has to
	// land there to reach the database.
	t.Run("forge disk strategy defaults on the nested payload", func(t *testing.T) {
		req := &api.CreateNodePoolRequest{Nico: &api.NicoNodePoolProvider{}}
		svc.applyNodePoolDefaults(req, forgeCluster)
		assert.Equal(t, api.DiskSelectionStrategyCreateSmallest, req.Nico.DiskSelectionStrategy)

		dbNodePool, err := dbNodePoolFromAPI(req, "1.29.7")
		require.NoError(t, err)
		nico, err := dbNodePool.NicoProvider()
		require.NoError(t, err)
		assert.Equal(t, db.DiskSelectionStrategySmallest, nico.DiskSelectionStrategy)
	})

	t.Run("explicit forge disk strategy preserved", func(t *testing.T) {
		req := &api.CreateNodePoolRequest{
			Nico: &api.NicoNodePoolProvider{DiskSelectionStrategy: api.DiskSelectionStrategyCreateLargest},
		}
		svc.applyNodePoolDefaults(req, forgeCluster)
		assert.Equal(t, api.DiskSelectionStrategyCreateLargest, req.Nico.DiskSelectionStrategy)
	})
}

func TestDbClusterFromAPI_TeleportDefault(t *testing.T) {
	baseReq := func(teleport *bool) *api.CreateClusterRequest {
		return &api.CreateClusterRequest{
			Name: "test",
			Nico: &api.NicoClusterProvider{
				OrgID:  "org-1",
				SiteID: uuid.New(),
				VpcID:  uuid.New(),
			},
			CniPlugin:                     "calico",
			ControlPlaneKubernetesVersion: "1.30.0",
			TeleportEnabled:               teleport,
		}
	}

	tests := []struct {
		name           string
		reqTeleport    *bool
		defaultEnabled bool
		wantTeleport   bool
	}{
		{
			name:           "nil request + default false => false",
			reqTeleport:    nil,
			defaultEnabled: false,
			wantTeleport:   false,
		},
		{
			name:           "nil request + default true => true",
			reqTeleport:    nil,
			defaultEnabled: true,
			wantTeleport:   true,
		},
		{
			name:           "explicit true + default false => true",
			reqTeleport:    ptrBool(true),
			defaultEnabled: false,
			wantTeleport:   true,
		},
		{
			name:           "explicit false + default true => false",
			reqTeleport:    ptrBool(false),
			defaultEnabled: true,
			wantTeleport:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &clusterService{
				teleportCfg: config.TeleportConfig{DefaultClusterEnabled: tt.defaultEnabled},
			}
			req := baseReq(tt.reqTeleport)
			cluster, err := DBClusterFromAPI(req)
			require.NoError(t, err)
			require.NoError(t, svc.applyClusterDefaults(req, &cluster))
			assert.Equal(t, tt.wantTeleport, cluster.TeleportEnabled)
		})
	}
}

func TestDbClusterFromAPI_CNIVersion(t *testing.T) {
	baseReq := func(plugin api.CreateClusterRequestCniPlugin, cniVersion *string) *api.CreateClusterRequest {
		return &api.CreateClusterRequest{
			Name: "test",
			Nico: &api.NicoClusterProvider{
				OrgID:  "org-1",
				SiteID: uuid.New(),
				VpcID:  uuid.New(),
			},
			CniPlugin:                     plugin,
			CniVersion:                    cniVersion,
			ControlPlaneKubernetesVersion: "1.30.0",
		}
	}

	tests := []struct {
		name       string
		plugin     api.CreateClusterRequestCniPlugin
		cniVersion *string
		want       string
	}{
		{
			name:   "defaults calico",
			plugin: api.CreateClusterRequestCniPluginCalico,
			want:   db.DefaultCalicoCNIVersion,
		},
		{
			name:   "defaults ovn",
			plugin: api.CreateClusterRequestCniPluginOvnKubernetes,
			want:   db.DefaultOvnKubernetesCNIVersion,
		},
		{
			name:       "preserves explicit value",
			plugin:     api.CreateClusterRequestCniPluginCalico,
			cniVersion: ptrString("3.31.5"),
			want:       "3.31.5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster, err := DBClusterFromAPI(baseReq(tt.plugin, tt.cniVersion))
			require.NoError(t, err)
			assert.Equal(t, tt.want, cluster.CniVersion)
		})
	}

	t.Run("omitted plugin defaults to ovn", func(t *testing.T) {
		cluster, err := DBClusterFromAPI(baseReq("", nil))
		require.NoError(t, err)
		assert.Equal(t, db.CNIOvnKubernetes, cluster.CniPlugin)
		assert.Equal(t, db.DefaultOvnKubernetesCNIVersion, cluster.CniVersion)
	})
}

func TestAPIClusterFromDB_CNIVersionDefaultsOldRows(t *testing.T) {
	got := APIClusterFromDB(&db.Cluster{
		ID:                uuid.New(),
		Name:              "test",
		CniPlugin:         db.CNICalico,
		KubernetesVersion: "1.30.0",
		Nico: &db.NicoClusterProvider{
			OrgID:  "org-1",
			SiteID: uuid.New(),
			VpcID:  uuid.New(),
		},
	})

	assert.Equal(t, db.DefaultCalicoCNIVersion, got.CniVersion)
}

func TestAPIClusterFromDB_CreatedAt(t *testing.T) {
	createdAt := time.Date(2026, time.July, 29, 6, 20, 6, 0, time.UTC)

	got := APIClusterFromDB(&db.Cluster{
		ID:                uuid.New(),
		Name:              "test",
		CniPlugin:         db.CNICalico,
		KubernetesVersion: "1.30.0",
		CreatedAt:         createdAt,
	})

	assert.Equal(t, createdAt, got.CreatedAt)
}

func TestDbNodePoolFromAPI_TeleportDefault(t *testing.T) {
	baseReq := func(teleport *bool) *api.CreateNodePoolRequest {
		return &api.CreateNodePoolRequest{
			ClusterID:      uuid.New(),
			Name:           "test-pool",
			NodeNamePrefix: "test",
			Nico: &api.NicoNodePoolProvider{
				SubnetIDs:      []uuid.UUID{uuid.New()},
				InstanceTypeID: uuid.New(),
				OSProfile:      "ubuntu",
				ConfigProfile:  "default",
			},
			DesiredNodeCount: 1,
			TeleportEnabled:  teleport,
		}
	}

	tests := []struct {
		name           string
		reqTeleport    *bool
		defaultEnabled bool
		wantTeleport   bool
	}{
		{
			name:           "nil request + default false => false",
			reqTeleport:    nil,
			defaultEnabled: false,
			wantTeleport:   false,
		},
		{
			name:           "nil request + default true => true",
			reqTeleport:    nil,
			defaultEnabled: true,
			wantTeleport:   true,
		},
		{
			name:           "explicit true + default false => true",
			reqTeleport:    ptrBool(true),
			defaultEnabled: false,
			wantTeleport:   true,
		},
		{
			name:           "explicit false + default true => false",
			reqTeleport:    ptrBool(false),
			defaultEnabled: true,
			wantTeleport:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &nodePoolService{
				teleportCfg: config.TeleportConfig{DefaultCPPoolEnabled: tt.defaultEnabled},
			}
			req := baseReq(tt.reqTeleport)
			req.ControlPlane = true
			svc.applyNodePoolDefaults(req, dbClusterWithSiteID(uuid.New()))
			nodePool, err := dbNodePoolFromAPI(req, "1.30.0")
			require.NoError(t, err)
			assert.Equal(t, tt.wantTeleport, nodePool.TeleportEnabled)
		})
	}
}

func dbClusterWithSiteID(siteID uuid.UUID) db.Cluster {
	return db.Cluster{Nico: &db.NicoClusterProvider{SiteID: siteID}}
}
