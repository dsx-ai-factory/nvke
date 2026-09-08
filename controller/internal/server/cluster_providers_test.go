// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/api"
)

func TestClusterProvidersService_ListClusterProviders(t *testing.T) {
	svc := NewClusterProvidersService([]string{"nico", "infra"})

	resp, err := svc.ListClusterProviders(context.Background(), api.ListClusterProvidersRequestObject{})
	require.NoError(t, err)

	got, ok := resp.(api.ListClusterProviders200JSONResponse)
	require.True(t, ok)
	require.Equal(t, []api.ClusterProvider{api.ClusterProviderNico, api.ClusterProviderInfra}, got.Providers)
}
