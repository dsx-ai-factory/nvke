// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/clients"
	capiclient "github.com/dsx-ai-factory/nke/controller/internal/clients/capi"
	"github.com/dsx-ai-factory/nke/controller/internal/clients/nico"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

func TestGetNewNodeProvider(t *testing.T) {
	t.Run("capi provider selects the capi client", func(t *testing.T) {
		provider, err := clients.GetNewNodeProvider(db.Cluster{ID: uuid.New(), ProviderType: db.ProviderTypeCAPI})
		require.NoError(t, err)
		assert.IsType(t, &capiclient.Client{}, provider)
	})

	t.Run("nico provider selects the nico client", func(t *testing.T) {
		provider, err := clients.GetNewNodeProvider(db.Cluster{ID: uuid.New(), ProviderType: db.ProviderTypeNico})
		require.NoError(t, err)
		assert.IsType(t, &nico.Client{}, provider)
	})

	t.Run("unsupported provider errors", func(t *testing.T) {
		_, err := clients.GetNewNodeProvider(db.Cluster{ID: uuid.New(), ProviderType: db.ProviderType("unknown")})
		require.Error(t, err)
	})
}
