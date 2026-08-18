// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sdk

import (
	"maps"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/otelx"
)

// ngcProductionServerURL and ngcStagingServerURL are the NGC API server URLs for
// the production and staging environments. The public SDK's NewConfiguration
// defaults to a single in-cluster server, so NewSDKConfig sets these explicitly
// to preserve the production/staging server-index semantics.
const (
	ngcProductionServerURL = "https://api.ngc.nvidia.com"
	ngcStagingServerURL    = "https://api.stg.ngc.nvidia.com"
)

// envServerIndexMap maps a ForgeEnvironment string to the corresponding SDK server index.
// NewSDKConfig sets the server list to production at 0 and staging at 1; we append the
// docker-test server and record its index so ContextServerIndex can select it.
var envServerIndexMap = func() map[string]int { //nolint:gochecknoglobals // computed once at init
	m := maps.Clone(ngcEnvServerIndexMap)
	m[config.ForgeEnvironmentTest] = len(m)
	return m
}()

// NewSDKConfig returns a Forge SDK configuration whose server list is
// production (index 0), staging (index 1), then cfg.ForgeURL as the docker-test
// entry. The server is selected at request time via ContextServerIndex.
//
// The public SDK's NewConfiguration defaults to a single in-cluster server, so we
// set the NGC production and staging servers explicitly rather than relying on the
// defaults, preserving the historical index semantics.
func NewSDKConfig(cfg *config.Config) *forgesdk.Configuration {
	sdkCfg := forgesdk.NewConfiguration()
	sdkCfg.Servers = forgesdk.ServerConfigurations{
		{
			URL:         ngcProductionServerURL,
			Description: "Production",
		},
		{
			URL:         ngcStagingServerURL,
			Description: "Staging",
		},
		{
			URL:         cfg.ForgeURL,
			Description: config.ForgeEnvironmentTest,
		},
	}
	if cfg.Tracing.Enabled {
		sdkCfg.HTTPClient = otelx.HTTPClient(sdkCfg.HTTPClient)
	}
	return sdkCfg
}

// ContextServerIndex returns the SDK server-list index for cfg.ForgeEnvironment.
// Pass the result as context.WithValue(ctx, forgesdk.ContextServerIndex, idx) before
// making any SDK API call.
func ContextServerIndex(cfg *config.Config) int {
	return envServerIndexMap[cfg.ForgeEnvironment]
}
