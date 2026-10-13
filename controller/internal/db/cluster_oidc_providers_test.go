// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func newOIDCProvidersTestCluster(ctx context.Context, t *testing.T, providers []db.ClusterOIDCProvider) (db.DB, db.Cluster) {
	t.Helper()
	q := db.NewDB(testutil.SetupDB(ctx, t, (*db.Cluster)(nil), (*db.ControlPlaneConfiguration)(nil)))
	cluster := newControlPlaneConfigTestCluster()
	cluster.OIDCProviders = providers
	created, err := q.CreateCluster(ctx, &cluster)
	require.NoError(t, err)
	return q, created
}

func corpProvider() db.ClusterOIDCProvider {
	return db.ClusterOIDCProvider{
		Name:           "corp",
		IssuerURL:      "https://issuer.example.com",
		Audiences:      []string{"nke"},
		UsernameClaim:  "sub",
		UsernamePrefix: "corp:",
		GroupsClaim:    "groups",
		GroupsPrefix:   "corp:",
		RequiredClaims: map[string]string{"tenant": "a"},
		CABundle:       "-----BEGIN CERTIFICATE-----\nexample\n-----END CERTIFICATE-----\n",
		DiscoveryURL:   "https://issuer.internal.example/.well-known/openid-configuration",
	}
}

func otherProvider() db.ClusterOIDCProvider {
	p := corpProvider()
	p.Name = "other"
	p.IssuerURL = "https://other.example.com"
	return p
}

// A field added to the file's provider and not to the stored one would be
// dropped from every snapshot without an error.
func TestClusterOIDCProviderHasEveryConfigField(t *testing.T) {
	fields := func(typ reflect.Type) map[string]string {
		out := map[string]string{}
		for i := range typ.NumField() {
			out[typ.Field(i).Name] = typ.Field(i).Tag.Get("json")
		}
		return out
	}
	assert.Equal(t,
		fields(reflect.TypeFor[controllerconfig.OIDCProvider]()),
		fields(reflect.TypeFor[db.ClusterOIDCProvider]()),
	)
}

func TestClusterOIDCProvidersFromConfig(t *testing.T) {
	t.Run("nil gives an empty list", func(t *testing.T) {
		got := db.ClusterOIDCProvidersFromConfig(nil)
		assert.NotNil(t, got)
		assert.Empty(t, got)
	})

	t.Run("copies every field without sharing slices or maps", func(t *testing.T) {
		want := corpProvider()
		in := []controllerconfig.OIDCProvider{{
			Name:           want.Name,
			IssuerURL:      want.IssuerURL,
			Audiences:      []string{"nke"},
			UsernameClaim:  want.UsernameClaim,
			UsernamePrefix: want.UsernamePrefix,
			GroupsClaim:    want.GroupsClaim,
			GroupsPrefix:   want.GroupsPrefix,
			RequiredClaims: map[string]string{"tenant": "a"},
			CABundle:       want.CABundle,
			DiscoveryURL:   want.DiscoveryURL,
		}}

		got := db.ClusterOIDCProvidersFromConfig(in)
		in[0].Audiences[0] = "changed"
		in[0].RequiredClaims["tenant"] = "changed"

		assert.Equal(t, []db.ClusterOIDCProvider{want}, got)
	})
}

func TestClusterOIDCProvidersToConfig(t *testing.T) {
	in := []controllerconfig.OIDCProvider{{
		Name:           "corp",
		IssuerURL:      "https://issuer.example.com",
		Audiences:      []string{"nke"},
		UsernameClaim:  "sub",
		UsernamePrefix: "corp:",
		GroupsClaim:    "groups",
		GroupsPrefix:   "corp:",
		RequiredClaims: map[string]string{"tenant": "a"},
		CABundle:       "pem",
		DiscoveryURL:   "https://issuer.internal.example/.well-known/openid-configuration",
	}}
	stored := db.ClusterOIDCProvidersFromConfig(in)
	got := db.ClusterOIDCProvidersToConfig(stored)
	assert.Equal(t, in, got)
	assert.Empty(t, db.ClusterOIDCProvidersToConfig(nil))

	got[0].Audiences[0] = "changed"
	got[0].RequiredClaims["tenant"] = "changed"
	assert.Equal(t, in, db.ClusterOIDCProvidersToConfig(stored), "the result must not alias the stored providers")
}

func TestClusterOIDCProvidersAtCreation(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name      string
		providers []db.ClusterOIDCProvider
	}{
		{name: "none pinned stays nil", providers: nil},
		{name: "an empty list stays empty, not nil", providers: []db.ClusterOIDCProvider{}},
		{name: "providers read back as created", providers: []db.ClusterOIDCProvider{corpProvider(), otherProvider()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, cluster := newOIDCProvidersTestCluster(ctx, t, tt.providers)
			got, err := q.GetCluster(ctx, cluster.ID)
			require.NoError(t, err)
			assert.Equal(t, tt.providers, got.OIDCProviders)
		})
	}
}

// The Postgres check constraint accepts SQL NULL or an array, not JSON null.
func TestClusterWithoutOIDCProvidersStoresNull(t *testing.T) {
	ctx := context.Background()
	bunDB := testutil.SetupDB(ctx, t, (*db.Cluster)(nil), (*db.ControlPlaneConfiguration)(nil))
	cluster := newControlPlaneConfigTestCluster()
	created, err := db.NewDB(bunDB).CreateCluster(ctx, &cluster)
	require.NoError(t, err)

	var isNull int
	require.NoError(t, bunDB.NewRaw("SELECT oidc_providers IS NULL FROM clusters WHERE id = ?", created.ID).Scan(ctx, &isNull))
	assert.Equal(t, 1, isNull)
}

func TestUpdateClusterKeepsPinnedOIDCProviders(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		stale []db.ClusterOIDCProvider
	}{
		{name: "a copy with no providers", stale: nil},
		{name: "a copy holding other providers", stale: []db.ClusterOIDCProvider{otherProvider()}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, cluster := newOIDCProvidersTestCluster(ctx, t, []db.ClusterOIDCProvider{corpProvider()})
			stale, err := q.GetCluster(ctx, cluster.ID)
			require.NoError(t, err)

			stale.OIDCProviders = tt.stale
			stale.KubernetesVersion = "1.35"
			updated, err := q.UpdateCluster(ctx, &stale)
			require.NoError(t, err)
			assert.Equal(t, []db.ClusterOIDCProvider{corpProvider()}, updated.OIDCProviders)

			got, err := q.GetCluster(ctx, cluster.ID)
			require.NoError(t, err)
			assert.Equal(t, "1.35", got.KubernetesVersion)
			assert.Equal(t, []db.ClusterOIDCProvider{corpProvider()}, got.OIDCProviders)
		})
	}
}
