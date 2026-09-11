// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/api"
)

func TestInfraProvidersService_ListInfraProviders(t *testing.T) {
	svc := NewInfraProvidersService([]string{"nico", "infra"})

	resp, err := svc.ListInfraProviders(context.Background(), api.ListInfraProvidersRequestObject{})
	require.NoError(t, err)

	got, ok := resp.(api.ListInfraProviders200JSONResponse)
	require.True(t, ok)
	require.Equal(t, []api.InfraProvider{api.InfraProviderNico, api.InfraProviderInfra}, got.Providers)
}
