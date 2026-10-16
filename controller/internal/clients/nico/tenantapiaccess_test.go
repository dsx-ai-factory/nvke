// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
)

func TestClient_TenantAPIAccessMethod(t *testing.T) {
	tests := map[string]struct {
		tenantAccessEnabled bool
		wantMethod          shared.TenantAPIAccessMethod
	}{
		"uses the published secret when tenant access is enabled": {
			tenantAccessEnabled: true,
			wantMethod:          shared.TenantAPIAccessSecret,
		},
		"falls back to the node agent when tenant access is disabled": {
			wantMethod: shared.TenantAPIAccessNodeAgent,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, params.wantMethod, New().TenantAPIAccessMethod(params.tenantAccessEnabled))
		})
	}
}
