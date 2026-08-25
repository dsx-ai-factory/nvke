// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sdk

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/config"
)

func TestContextServerIndex(t *testing.T) {
	tests := []struct {
		name        string
		environment string
		wantIndex   int
	}{
		{
			name:        "production uses index 0",
			environment: config.ForgeEnvironmentProduction,
			wantIndex:   0,
		},
		{
			name:        "test uses a non-production index",
			environment: config.ForgeEnvironmentTest,
			wantIndex:   envServerIndexMap[config.ForgeEnvironmentTest],
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{ForgeEnvironment: tt.environment}
			got := ContextServerIndex(cfg)
			require.Equal(t, tt.wantIndex, got)
		})
	}

	// docker-test and production must be distinct
	prodIdx := ContextServerIndex(&config.Config{ForgeEnvironment: config.ForgeEnvironmentProduction})
	dockerIdx := ContextServerIndex(&config.Config{ForgeEnvironment: config.ForgeEnvironmentTest})
	require.NotEqual(t, prodIdx, dockerIdx, "production and docker-test must map to different server indices")
}

func TestNewSDKConfig_AppendsDockerTestServer(t *testing.T) {
	cfg := &config.Config{
		ForgeEnvironment: config.ForgeEnvironmentTest,
		ForgeURL:         "http://localhost:9090",
	}
	sdkCfg := NewSDKConfig(cfg)

	// The appended entry should be at the docker-test index
	idx := ContextServerIndex(cfg)
	require.Less(t, idx, len(sdkCfg.Servers), "docker-test server index must be within server list bounds")

	url, err := sdkCfg.Servers.URL(idx, nil)
	require.NoError(t, err)
	require.Equal(t, "http://localhost:9090", url)

	// Invariant: the configured server list must have exactly as many entries as
	// envServerIndexMap. If a server is added without a matching map entry (or vice
	// versa), the docker-test index computed via len(map) would be wrong and
	// silently point to the wrong server.
	require.Len(t, sdkCfg.Servers, len(envServerIndexMap),
		"SDK server list length must equal envServerIndexMap size (including docker-test entry); "+
			"if the upstream SDK added or removed a server, update envServerIndexMap accordingly")
}

func TestNewSDKConfig_SetsForgeAPIName(t *testing.T) {
	sdkCfg := NewSDKConfig(&config.Config{})
	require.Equal(t, "forge", sdkCfg.GetAPIName())
}
