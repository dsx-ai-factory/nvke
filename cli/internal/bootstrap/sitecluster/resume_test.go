// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitecluster

import (
	"context"
	"testing"

	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
	api "github.com/dsx-ai-factory/nvke/controller/api"
)

// builtFrom is the cluster the controller holds after creating inv's request.
func builtFrom(t *testing.T, inv *sitespec.SiteSpec) *api.Cluster {
	t.Helper()
	req, err := ClusterRequest(inv)
	require.NoError(t, err)
	built := &api.Cluster{
		Name:                          req.Name,
		ControlPlaneKubernetesVersion: req.ControlPlaneKubernetesVersion,
		CniPlugin:                     string(req.CniPlugin),
		EtcdEncrypted:                 req.EtcdEncrypted,
		ControlPlaneVIP:               &req.ControlPlaneVIP,
		Nico:                          req.Nico,
		ControlPlaneConfig:            &api.ControlPlaneConfig{DesiredCount: req.ControlPlaneConfig.DesiredCount},
	}
	if req.CniVersion != nil {
		built.CniVersion = *req.CniVersion
	}
	if req.Network != nil {
		built.Network = api.ClusterNetwork{PodCIDR: req.Network.PodCIDR, ServiceCIDR: req.Network.ServiceCIDR}
	}
	return built
}

func TestCheckUnchanged(t *testing.T) {
	tests := []struct {
		name    string
		change  func(inv *sitespec.SiteSpec)
		stored  string // the pod CIDR the controller stored, when it differs from the spec's text
		wantErr string
	}{
		{name: "the same site spec", change: func(*sitespec.SiteSpec) {}},
		{name: "a safe field changed", change: func(inv *sitespec.SiteSpec) { inv.Infra.Nico.Remote.Endpoint = "https://other.example" }},
		{
			name:   "CIDRs written other than as stored",
			change: func(inv *sitespec.SiteSpec) { inv.Cluster.PodCidr = "10.244.1.0/16, 10.245.0.0/16" },
			stored: "10.244.0.0/16,10.245.0.0/16",
		},
		{
			name:    "a new control-plane VIP",
			change:  func(inv *sitespec.SiteSpec) { inv.Cluster.ControlPlaneVIP = "192.0.2.99" },
			wantErr: "cluster.controlPlaneVIP is 192.0.2.99 in the site spec but",
		},
		{
			name:    "another Kubernetes version",
			change:  func(inv *sitespec.SiteSpec) { inv.Cluster.K8sVersion = "1.99.0" },
			wantErr: "cluster.k8sVersion is 1.99.0",
		},
		{
			name:    "another NICo site",
			change:  func(inv *sitespec.SiteSpec) { inv.Infra.Nico.Remote.SiteID = openapi_types.UUID{15: 9} },
			wantErr: "infra.nico.remote.siteID",
		},
		{
			name:    "another control-plane count",
			change:  func(inv *sitespec.SiteSpec) { inv.Infra.Nico.ControlPlane.Count = 5 },
			wantErr: "the control-plane node count is 5 in the site spec but 3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			built := builtFrom(t, nicoSiteSpec())
			if tt.stored != "" {
				built.Network.PodCIDR = tt.stored
			}
			inv := nicoSiteSpec()
			tt.change(inv)
			err := CheckUnchanged(inv, built)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestFind(t *testing.T) {
	inv := nicoSiteSpec()
	other := api.Cluster{Name: inv.ClusterName + "-other"}
	ours := api.Cluster{Name: inv.ClusterName, ID: openapi_types.UUID{15: 7}}

	found, err := Find(context.Background(), &fakeClusterAPI{listed: []api.Cluster{other}}, inv)
	require.NoError(t, err)
	require.Nil(t, found, "a cluster of another name is not this one")

	found, err = Find(context.Background(), &fakeClusterAPI{listed: []api.Cluster{other, ours}}, inv)
	require.NoError(t, err)
	require.Equal(t, ours.ID, found.ID)
}
