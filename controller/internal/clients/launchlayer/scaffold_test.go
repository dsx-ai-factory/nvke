// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClient_GetClusterScaffoldManifest(t *testing.T) {
	cluster := db.Cluster{Name: "test-cluster"}
	cluster.SetLaunchLayerProvider(db.LaunchLayerClusterProvider{
		OrgID:   "org-1",
		VpcType: "VIRTUALIZED",
		SiteID:  "site-1",
		Owner:   db.LaunchLayerClusterOwner{UserID: "user-1"},
		SecurityGroups: []db.LaunchLayerSecurityGroup{
			{Name: "control-plane", Type: "VIRTUALIZED", NetworkType: "primary"},
		},
	})

	raw, err := New().GetClusterScaffoldManifest(cluster)
	require.NoError(t, err)

	var manifest map[string]any
	require.NoError(t, json.Unmarshal(raw, &manifest))
	require.Equal(t, "infrastructure.cluster.x-k8s.io/v1alpha1", manifest["apiVersion"])
	require.Equal(t, "LaunchLayerCluster", manifest["kind"])

	spec, ok := manifest["spec"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "org-1", spec["orgID"])
	require.Equal(t, "VIRTUALIZED", spec["vpcType"])
	require.Equal(t, "site-1", spec["siteID"])
	require.NotContains(t, spec, "vpcID")

	owner, ok := spec["owner"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "user-1", owner["userID"])

	groups, ok := spec["securityGroups"].([]any)
	require.True(t, ok)
	require.Len(t, groups, 1)
}

func TestClient_GetClusterScaffoldManifest_MissingPayload(t *testing.T) {
	var cluster db.Cluster
	cluster.SetNicoProvider(db.NicoClusterProvider{})

	_, err := New().GetClusterScaffoldManifest(cluster)

	require.Error(t, err)
	require.Contains(t, err.Error(), `provider type is "nico", not "launchlayer"`)
}
