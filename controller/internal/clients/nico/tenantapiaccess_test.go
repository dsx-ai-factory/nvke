// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClient_TenantAPIAccessMethod(t *testing.T) {
	cluster := db.Cluster{ID: uuid.New()}
	cluster.SetNicoProvider(db.NicoClusterProvider{OrgID: "org-1", SiteID: uuid.New(), VpcID: uuid.New()})

	type parameters struct {
		cluster             db.Cluster
		tenantAccessEnabled bool
		wantMethod          shared.TenantAPIAccessMethod
		wantErr             error
	}

	tests := map[string]parameters{
		"uses the published secret when tenant access is enabled": {
			cluster:             cluster,
			tenantAccessEnabled: true,
			wantMethod:          shared.TenantAPIAccessSecret,
		},
		"falls back to the node agent when tenant access is disabled": {
			cluster:    cluster,
			wantMethod: shared.TenantAPIAccessNodeAgent,
		},
		"errors without a nico payload": {
			cluster:             db.Cluster{ID: uuid.New(), ProviderType: db.ProviderTypeNico},
			tenantAccessEnabled: true,
			wantErr:             db.ErrMissingProviderPayload,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			method, err := New().TenantAPIAccessMethod(params.cluster, params.tenantAccessEnabled)
			if params.wantErr != nil {
				require.ErrorIs(t, err, params.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, params.wantMethod, method)
		})
	}
}
