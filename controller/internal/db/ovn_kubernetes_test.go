// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestClusterOVNKubernetesPersistence(t *testing.T) {
	for _, tt := range []struct {
		name   string
		config *db.OVNKubernetesConfig
	}{
		{name: "omitted"},
		{name: "empty", config: &db.OVNKubernetesConfig{}},
		{name: "explicit false", config: &db.OVNKubernetesConfig{AllowNoUplink: testutil.PtrTo(false)}},
		{
			name: "all settings",
			config: &db.OVNKubernetesConfig{
				GatewayInterface: testutil.PtrTo("brfab0"),
				AllowNoUplink:    testutil.PtrTo(true),
				GatewayMode:      testutil.PtrTo("shared"),
				MTU:              testutil.PtrTo(1400),
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			queries := db.NewDB(testutil.SetupDB(ctx, t, (*db.Cluster)(nil)))
			cluster := newValidCluster()
			cluster.CniPlugin = db.CNIOvnKubernetes
			cluster.OVNKubernetes = tt.config
			_, err := queries.CreateCluster(ctx, &cluster)
			require.NoError(t, err)

			stored, err := queries.GetCluster(ctx, cluster.ID)
			require.NoError(t, err)
			require.Equal(t, tt.config, stored.OVNKubernetes)

			stored.Name += "-updated"
			_, err = queries.UpdateCluster(ctx, &stored)
			require.NoError(t, err)
			updated, err := queries.GetCluster(ctx, cluster.ID)
			require.NoError(t, err)
			require.Equal(t, tt.config, updated.OVNKubernetes)
		})
	}
}
