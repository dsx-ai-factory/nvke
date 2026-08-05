// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/db"
)

func TestResolveClusterInfraMode(t *testing.T) {
	t.Run("forge cluster when siteID is non-nil and capi cluster manifest is empty", func(t *testing.T) {
		mode, err := ResolveClusterInfraMode(db.Cluster{
			ID:               uuid.New(),
			SiteID:           uuid.New(),
			CapiInfraCluster: json.RawMessage(`{}`),
		}, false)
		require.NoError(t, err)
		assert.Equal(t, InfraModeForge, mode)
	})

	t.Run("forge cluster when siteID is non-nil and capi cluster manifest is null", func(t *testing.T) {
		mode, err := ResolveClusterInfraMode(db.Cluster{
			ID:               uuid.New(),
			SiteID:           uuid.New(),
			CapiInfraCluster: json.RawMessage(`null`),
		}, false)
		require.NoError(t, err)
		assert.Equal(t, InfraModeForge, mode)
	})

	t.Run("capi cluster when siteID is nil and capi cluster manifest is present", func(t *testing.T) {
		mode, err := ResolveClusterInfraMode(db.Cluster{
			ID:               uuid.New(),
			SiteID:           uuid.Nil,
			CapiInfraCluster: json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeCluster"}`),
		}, false)
		require.NoError(t, err)
		assert.Equal(t, InfraModeCAPI, mode)
	})

	t.Run("errors when siteID is nil without capi cluster manifest", func(t *testing.T) {
		_, err := ResolveClusterInfraMode(db.Cluster{
			ID:               uuid.New(),
			SiteID:           uuid.Nil,
			CapiInfraCluster: json.RawMessage(`{}`),
		}, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must define capiInfraCluster")
	})

	t.Run("forge cluster when siteID is non-nil and capi cluster manifest is present", func(t *testing.T) {
		mode, err := ResolveClusterInfraMode(db.Cluster{
			ID:               uuid.New(),
			SiteID:           uuid.New(),
			CapiInfraCluster: json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeCluster"}`),
		}, false)
		require.NoError(t, err)
		assert.Equal(t, InfraModeForge, mode)
	})

	t.Run("forge cluster uses CAPI when forge CAPI is enabled and capi cluster manifest is present", func(t *testing.T) {
		mode, err := ResolveClusterInfraMode(db.Cluster{
			ID:               uuid.New(),
			SiteID:           uuid.New(),
			CapiInfraCluster: json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1alpha1","kind":"NicoCluster"}`),
		}, true)
		require.NoError(t, err)
		assert.Equal(t, InfraModeCAPI, mode)
	})
}

func TestResolveNodeInfraMode(t *testing.T) {
	clusterID := uuid.New()
	nodeSetID := uuid.New()

	t.Run("capi node set when machine template is present", func(t *testing.T) {
		mode, err := ResolveNodeInfraMode(
			db.Cluster{
				ID:               clusterID,
				SiteID:           uuid.Nil,
				CapiInfraCluster: json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeCluster"}`),
			},
			&db.NodeSet{
				ID:                       nodeSetID,
				CapiInfraMachineTemplate: json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeMachineTemplate"}`),
			},
			false,
		)
		require.NoError(t, err)
		assert.Equal(t, InfraModeCAPI, mode)
	})

	t.Run("forge node set when cluster siteID is non-nil and machine template is empty", func(t *testing.T) {
		mode, err := ResolveNodeInfraMode(
			db.Cluster{
				ID:               clusterID,
				SiteID:           uuid.New(),
				CapiInfraCluster: json.RawMessage(`{}`),
			},
			&db.NodeSet{
				ID:                       nodeSetID,
				CapiInfraMachineTemplate: json.RawMessage(`{}`),
			},
			false,
		)
		require.NoError(t, err)
		assert.Equal(t, InfraModeForge, mode)
	})

	t.Run("forge node set when manifests are null", func(t *testing.T) {
		mode, err := ResolveNodeInfraMode(
			db.Cluster{
				ID:               clusterID,
				SiteID:           uuid.New(),
				CapiInfraCluster: json.RawMessage(`null`),
			},
			&db.NodeSet{
				ID:                       nodeSetID,
				CapiInfraMachineTemplate: json.RawMessage(`null`),
			},
			false,
		)
		require.NoError(t, err)
		assert.Equal(t, InfraModeForge, mode)
	})

	t.Run("forge node set uses capi when machine template is present", func(t *testing.T) {
		mode, err := ResolveNodeInfraMode(
			db.Cluster{
				ID:               clusterID,
				SiteID:           uuid.New(),
				CapiInfraCluster: json.RawMessage(`{}`),
			},
			&db.NodeSet{
				ID:                       nodeSetID,
				CapiInfraMachineTemplate: json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeMachineTemplate"}`),
			},
			true,
		)
		require.NoError(t, err)
		assert.Equal(t, InfraModeCAPI, mode)
	})

	t.Run("errors when capi-backed cluster omits machine template", func(t *testing.T) {
		_, err := ResolveNodeInfraMode(
			db.Cluster{
				ID:               clusterID,
				SiteID:           uuid.Nil,
				CapiInfraCluster: json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeCluster"}`),
			},
			&db.NodeSet{
				ID:                       nodeSetID,
				CapiInfraMachineTemplate: json.RawMessage(`{}`),
			},
			false,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must define capiInfraMachineTemplate")
	})

	t.Run("forge no-nodepool path when siteID is non-nil and capi cluster manifest is present", func(t *testing.T) {
		mode, err := ResolveNodeInfraMode(
			db.Cluster{
				ID:               clusterID,
				SiteID:           uuid.New(),
				CapiInfraCluster: json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeCluster"}`),
			},
			nil,
			false,
		)
		require.NoError(t, err)
		assert.Equal(t, InfraModeForge, mode)
	})

	t.Run("errors when node context is missing machine template for capi-backed cluster", func(t *testing.T) {
		_, err := ResolveNodeInfraMode(
			db.Cluster{
				ID:               clusterID,
				SiteID:           uuid.Nil,
				CapiInfraCluster: json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeCluster"}`),
			},
			nil,
			false,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must define capiInfraMachineTemplate")
	})

	t.Run("no-nodepool path uses resolved node context template", func(t *testing.T) {
		mode, err := ResolveNodeInfraMode(
			db.Cluster{
				ID:               clusterID,
				SiteID:           uuid.Nil,
				CapiInfraCluster: json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeCluster"}`),
				ControlPlaneConfig: db.ControlPlaneConfig{
					ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
						InfraMachineTemplatePreferences: []json.RawMessage{
							json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"DockerMachineTemplate"}`),
						},
					},
				},
			},
			&db.NodeSet{
				ID:                       uuid.Nil,
				CapiInfraMachineTemplate: json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"ForgeMachineTemplate"}`),
			},
			false,
		)
		require.NoError(t, err)
		assert.Equal(t, InfraModeCAPI, mode)
	})

	t.Run("forge node set uses capi when forge capi is enabled and machine template is empty", func(t *testing.T) {
		mode, err := ResolveNodeInfraMode(
			db.Cluster{
				ID:               clusterID,
				SiteID:           uuid.New(),
				CapiInfraCluster: json.RawMessage(`{}`),
			},
			&db.NodeSet{
				ID:                       nodeSetID,
				CapiInfraMachineTemplate: json.RawMessage(`{}`),
			},
			true,
		)
		require.NoError(t, err)
		assert.Equal(t, InfraModeCAPI, mode)
	})

	t.Run("forge node set with default capi cluster manifest uses capi when forge capi is enabled and machine template is empty", func(t *testing.T) {
		mode, err := ResolveNodeInfraMode(
			db.Cluster{
				ID:               clusterID,
				SiteID:           uuid.New(),
				CapiInfraCluster: json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1alpha1","kind":"NicoCluster"}`),
			},
			&db.NodeSet{
				ID:                       nodeSetID,
				CapiInfraMachineTemplate: json.RawMessage(`{}`),
			},
			true,
		)
		require.NoError(t, err)
		assert.Equal(t, InfraModeCAPI, mode)
	})

	t.Run("forge no-nodepool path uses capi when forge capi is enabled and machine template is empty", func(t *testing.T) {
		mode, err := ResolveNodeInfraMode(
			db.Cluster{
				ID:               clusterID,
				SiteID:           uuid.New(),
				CapiInfraCluster: json.RawMessage(`{}`),
			},
			nil,
			true,
		)
		require.NoError(t, err)
		assert.Equal(t, InfraModeCAPI, mode)
	})
}
