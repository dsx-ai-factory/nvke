// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/capi"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClient_GetClusterScaffoldManifest(t *testing.T) {
	t.Run("returns the caller-supplied infra cluster template", func(t *testing.T) {
		want := json.RawMessage(`{"kind":"ForgeCluster"}`)
		cluster := db.Cluster{
			ID:           uuid.New(),
			ProviderType: db.ProviderTypeCAPI,
			Capi:         &db.CAPIClusterProvider{InfraCluster: want},
		}

		got, err := capi.New().GetClusterScaffoldManifest(cluster)
		require.NoError(t, err)
		require.JSONEq(t, string(want), string(got))
	})

	t.Run("missing provider payload errors", func(t *testing.T) {
		_, err := capi.New().GetClusterScaffoldManifest(db.Cluster{
			ID:           uuid.New(),
			ProviderType: db.ProviderTypeCAPI,
		})
		require.Error(t, err)
		require.ErrorIs(t, err, db.ErrMissingProviderPayload)
	})
}
