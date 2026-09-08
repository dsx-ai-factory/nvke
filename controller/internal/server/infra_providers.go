// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	"github.com/NVIDIA/nke/controller/api"
)

type infraProvidersService struct {
	providers []api.InfraProvider
}

func NewInfraProvidersService(providers []string) *infraProvidersService {
	return &infraProvidersService{
		providers: apiInfraProvidersFromConfig(providers),
	}
}

func (s *infraProvidersService) ListInfraProviders(
	_ context.Context,
	_ api.ListInfraProvidersRequestObject,
) (api.ListInfraProvidersResponseObject, error) {
	return api.ListInfraProviders200JSONResponse{Providers: s.providers}, nil
}

func apiInfraProvidersFromConfig(providers []string) []api.InfraProvider {
	apiProviders := make([]api.InfraProvider, len(providers))
	for i, provider := range providers {
		apiProviders[i] = api.InfraProvider(provider)
	}
	return apiProviders
}
