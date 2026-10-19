// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
)

func TestClient_TenantAPIAccessMethod(t *testing.T) {
	for _, tenantAccessEnabled := range []bool{true, false} {
		require.Equal(t, shared.TenantAPIAccessSecret, New().TenantAPIAccessMethod(tenantAccessEnabled))
	}
}
