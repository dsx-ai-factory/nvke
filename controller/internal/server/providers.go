// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
)

type providerSupport struct {
	supported map[string]struct{}
}

func newProviderSupport(providers []string) providerSupport {
	if len(providers) == 0 {
		providers = []string{config.InfraProviderNico, config.InfraProviderInfra}
	}
	supported := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		supported[provider] = struct{}{}
	}
	return providerSupport{supported: supported}
}

func (p providerSupport) supports(provider string) bool {
	_, ok := p.effective().supported[provider]
	return ok
}

func (p providerSupport) require(provider string) error {
	if p.supports(provider) {
		return nil
	}
	return fmt.Errorf("%s provider is not supported by this deployment", provider)
}

func (p providerSupport) effective() providerSupport {
	if p.supported != nil {
		return p
	}
	return newProviderSupport(nil)
}
