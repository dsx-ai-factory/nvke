// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
)

func TestParseConfigFromEnvRequiresControllerConfigPath(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		t.Setenv("CONTROLLER_CONFIG_PATH", "")
		require.NoError(t, os.Unsetenv("CONTROLLER_CONFIG_PATH"))

		_, err := config.ParseConfigFromEnv()
		require.ErrorContains(t, err, "CONTROLLER_CONFIG_PATH")
	})
	t.Run("empty", func(t *testing.T) {
		t.Setenv("CONTROLLER_CONFIG_PATH", "")

		_, err := config.ParseConfigFromEnv()
		require.ErrorContains(t, err, "CONTROLLER_CONFIG_PATH")
	})
}

// A variable whose setting moved into nke-controller-config no longer reaches
// config.Config, so a stale override cannot quietly win over the file.
func TestParseConfigFromEnvIgnoresRetiredVariables(t *testing.T) {
	setMinimumEnv(t)
	t.Setenv("TEMPORAL_HOST_PORT", "temporal.example.test:7233")
	t.Setenv("NKE_OIDC_AUTH_ENABLED", "true")
	t.Setenv("PERMISSIVE_MODE", "true")

	cfg, err := config.ParseConfigFromEnv()
	require.NoError(t, err)
	assert.Empty(t, cfg.TemporalHostPort)
	assert.False(t, cfg.NvkeOIDCAuth.Enabled)
	assert.False(t, cfg.PermissiveMode)
}
