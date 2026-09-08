// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	"github.com/NVIDIA/nke/controller/api"
)

type clusterProvidersService struct {
	providers []api.ClusterProvider
}

func NewClusterProvidersService(providers []string) *clusterProvidersService {
	return &clusterProvidersService{
		providers: apiClusterProvidersFromConfig(providers),
	}
}

func (s *clusterProvidersService) ListClusterProviders(
	_ context.Context,
	_ api.ListClusterProvidersRequestObject,
) (api.ListClusterProvidersResponseObject, error) {
	return api.ListClusterProviders200JSONResponse{Providers: s.providers}, nil
}

func apiClusterProvidersFromConfig(providers []string) []api.ClusterProvider {
	apiProviders := make([]api.ClusterProvider, len(providers))
	for i, provider := range providers {
		apiProviders[i] = api.ClusterProvider(provider)
	}
	return apiProviders
}
