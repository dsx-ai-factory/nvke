// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClient_TenantAPIAccessMethod(t *testing.T) {
	withInfraCluster := func(infraCluster string) db.Cluster {
		cluster := db.Cluster{ID: uuid.New()}
		cluster.SetCAPIProvider(db.CAPIClusterProvider{InfraCluster: []byte(infraCluster)})
		return cluster
	}

	type parameters struct {
		cluster             db.Cluster
		tenantAccessEnabled bool
		wantMethod          shared.TenantAPIAccessMethod
		wantErrIs           error
		wantErr             string
	}

	tests := map[string]parameters{
		"uses the published secret when tenant access is enabled": {
			cluster:             withInfraCluster(`{"kind":"DockerCluster"}`),
			tenantAccessEnabled: true,
			wantMethod:          shared.TenantAPIAccessSecret,
		},
		"uses the published secret when tenant access is disabled": {
			cluster:    withInfraCluster(`{"kind":"DockerCluster"}`),
			wantMethod: shared.TenantAPIAccessSecret,
		},
		"errors when infra.cluster is empty": {
			cluster: withInfraCluster(`{}`),
			wantErr: "must define infra.cluster",
		},
		"errors without a capi payload": {
			cluster:   db.Cluster{ID: uuid.New(), ProviderType: db.ProviderTypeCAPI},
			wantErrIs: db.ErrMissingProviderPayload,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			method, err := New().TenantAPIAccessMethod(params.cluster, params.tenantAccessEnabled)
			switch {
			case params.wantErrIs != nil:
				require.ErrorIs(t, err, params.wantErrIs)
			case params.wantErr != "":
				require.ErrorContains(t, err, params.wantErr)
			default:
				require.NoError(t, err)
				require.Equal(t, params.wantMethod, method)
			}
		})
	}
}
