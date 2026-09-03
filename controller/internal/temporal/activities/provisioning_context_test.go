// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/db"
)

func nicoCluster(id uuid.UUID) db.Cluster {
	siteID := uuid.New()
	vpcID := uuid.New()
	cluster := db.Cluster{
		ID:           id,
		OrgID:        "org-1",
		SiteID:       siteID,
		VpcID:        vpcID,
		ProviderType: db.ProviderTypeNico,
		Nico:         &db.NicoClusterProvider{OrgID: "org-1", SiteID: siteID, VpcID: vpcID},
	}
	return cluster
}

func capiCluster(id uuid.UUID, manifest json.RawMessage) db.Cluster {
	return db.Cluster{
		ID:           id,
		SiteID:       uuid.Nil,
		ProviderType: db.ProviderTypeCAPI,
		Capi:         &db.CAPIClusterProvider{InfraCluster: manifest},
	}
}

func TestResolveClusterInfraMode(t *testing.T) {
	t.Run("nico cluster uses CAPI when siteID is non-nil and capi cluster manifest is empty", func(t *testing.T) {
		mode, err := ResolveClusterInfraMode(nicoCluster(uuid.New()))
		require.NoError(t, err)
		assert.Equal(t, InfraModeCAPI, mode)
	})

	t.Run("nico cluster uses CAPI when siteID is non-nil and capi cluster manifest is null", func(t *testing.T) {
		mode, err := ResolveClusterInfraMode(nicoCluster(uuid.New()))
		require.NoError(t, err)
		assert.Equal(t, InfraModeCAPI, mode)
	})

	t.Run("capi cluster when siteID is nil and capi cluster manifest is present", func(t *testing.T) {
		mode, err := ResolveClusterInfraMode(capiCluster(uuid.New(), json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeCluster"}`)))
		require.NoError(t, err)
		assert.Equal(t, InfraModeCAPI, mode)
	})

	t.Run("errors when siteID is nil without capi cluster manifest", func(t *testing.T) {
		_, err := ResolveClusterInfraMode(capiCluster(uuid.New(), json.RawMessage(`{}`)))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must define capiInfraCluster")
	})

	t.Run("nico cluster uses CAPI when siteID is non-nil and capi cluster manifest is present", func(t *testing.T) {
		mode, err := ResolveClusterInfraMode(nicoCluster(uuid.New()))
		require.NoError(t, err)
		assert.Equal(t, InfraModeCAPI, mode)
	})
}

func TestResolveNodeInfraMode(t *testing.T) {
	clusterID := uuid.New()
	nodeSetID := uuid.New()

	t.Run("capi node set when machine template is present", func(t *testing.T) {
		nodeSet := db.NodeSet{ID: nodeSetID}
		nodeSet.SetCapi(db.CAPINodePoolProvider{
			InfraMachineTemplate: json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeMachineTemplate"}`),
		})
		mode, err := ResolveNodeInfraMode(
			capiCluster(clusterID, json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeCluster"}`)),
			&nodeSet,
		)
		require.NoError(t, err)
		assert.Equal(t, InfraModeCAPI, mode)
	})

	t.Run("nico node set uses CAPI when cluster siteID is non-nil and machine template is empty", func(t *testing.T) {
		mode, err := ResolveNodeInfraMode(
			nicoCluster(clusterID),
			&db.NodeSet{
				ID:                       nodeSetID,
				CapiInfraMachineTemplate: json.RawMessage(`{}`),
			},
		)
		require.NoError(t, err)
		assert.Equal(t, InfraModeCAPI, mode)
	})

	t.Run("nico node set uses CAPI when manifests are null", func(t *testing.T) {
		mode, err := ResolveNodeInfraMode(
			nicoCluster(clusterID),
			&db.NodeSet{
				ID:                       nodeSetID,
				CapiInfraMachineTemplate: json.RawMessage(`null`),
			},
		)
		require.NoError(t, err)
		assert.Equal(t, InfraModeCAPI, mode)
	})

	t.Run("nico node set uses CAPI when machine template is present", func(t *testing.T) {
		mode, err := ResolveNodeInfraMode(
			nicoCluster(clusterID),
			&db.NodeSet{
				ID:                       nodeSetID,
				CapiInfraMachineTemplate: json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeMachineTemplate"}`),
			},
		)
		require.NoError(t, err)
		assert.Equal(t, InfraModeCAPI, mode)
	})

	t.Run("errors when capi-backed cluster omits machine template", func(t *testing.T) {
		nodeSet := db.NodeSet{ID: nodeSetID}
		nodeSet.SetCapi(db.CAPINodePoolProvider{InfraMachineTemplate: json.RawMessage(`{}`)})
		_, err := ResolveNodeInfraMode(
			capiCluster(clusterID, json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeCluster"}`)),
			&nodeSet,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must define capiInfraMachineTemplate")
	})

	t.Run("nico no-nodepool path uses CAPI", func(t *testing.T) {
		mode, err := ResolveNodeInfraMode(
			nicoCluster(clusterID),
			nil,
		)
		require.NoError(t, err)
		assert.Equal(t, InfraModeCAPI, mode)
	})

	t.Run("errors when node context is missing machine template for capi-backed cluster", func(t *testing.T) {
		_, err := ResolveNodeInfraMode(
			capiCluster(clusterID, json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeCluster"}`)),
			nil,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must define capiInfraMachineTemplate")
	})

	t.Run("no-nodepool path uses resolved node context template", func(t *testing.T) {
		cluster := capiCluster(clusterID, json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeCluster"}`))
		cluster.ControlPlaneConfig = db.ControlPlaneConfig{
			ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
				InfraMachineTemplatePreferences: []json.RawMessage{
					json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"DockerMachineTemplate"}`),
				},
			},
		}
		nodeSet := db.NodeSet{ID: uuid.Nil}
		nodeSet.SetCapi(db.CAPINodePoolProvider{
			InfraMachineTemplate: json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeMachineTemplate"}`),
		})
		mode, err := ResolveNodeInfraMode(
			cluster,
			&nodeSet,
		)
		require.NoError(t, err)
		assert.Equal(t, InfraModeCAPI, mode)
	})

	t.Run("nico node set uses CAPI when machine template is empty", func(t *testing.T) {
		mode, err := ResolveNodeInfraMode(
			nicoCluster(clusterID),
			&db.NodeSet{
				ID:                       nodeSetID,
				CapiInfraMachineTemplate: json.RawMessage(`{}`),
			},
		)
		require.NoError(t, err)
		assert.Equal(t, InfraModeCAPI, mode)
	})
}
