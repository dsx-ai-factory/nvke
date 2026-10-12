// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients"
	capiclient "github.com/dsx-ai-factory/nvke/controller/internal/clients/capi"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/nico"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestGetClusterScaffold(t *testing.T) {
	t.Run("capi provider selects the capi client", func(t *testing.T) {
		want := json.RawMessage(`{"kind":"ForgeCluster"}`)
		scaffold, err := clients.GetClusterScaffold(db.Cluster{ID: uuid.New(), ProviderType: db.ProviderTypeCAPI})
		require.NoError(t, err)
		assert.IsType(t, &capiclient.Client{}, scaffold)

		got, err := scaffold.GetClusterScaffoldManifest(db.Cluster{
			ID:           uuid.New(),
			ProviderType: db.ProviderTypeCAPI,
			Capi:         &db.CAPIClusterProvider{InfraCluster: want},
		})
		require.NoError(t, err)
		require.JSONEq(t, string(want), string(got))
	})

	t.Run("nico provider selects the nico client", func(t *testing.T) {
		scaffold, err := clients.GetClusterScaffold(db.Cluster{ID: uuid.New(), ProviderType: db.ProviderTypeNico})
		require.NoError(t, err)
		assert.IsType(t, &nico.Client{}, scaffold)
	})

	t.Run("unsupported provider errors", func(t *testing.T) {
		_, err := clients.GetClusterScaffold(db.Cluster{ID: uuid.New(), ProviderType: db.ProviderType("unknown")})
		require.Error(t, err)
	})
}
