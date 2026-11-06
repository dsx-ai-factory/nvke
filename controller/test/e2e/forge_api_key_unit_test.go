// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra/forge"
)

func TestForgeAPIKey(t *testing.T) {
	t.Setenv("FORGE_API_KEY", "default-key")
	t.Setenv("FORGE_PERF_API_KEY", "perf-key")

	t.Log("Testing: an environment that names no variable reads FORGE_API_KEY")
	key, err := forgeAPIKey("forge", &forge.Environment{})
	require.NoError(t, err)
	require.Equal(t, "default-key", key)

	t.Log("Testing: an environment that names a variable reads that one")
	key, err = forgeAPIKey("forge-perf", &forge.Environment{APIKeyEnv: "FORGE_PERF_API_KEY"})
	require.NoError(t, err)
	require.Equal(t, "perf-key", key)

	t.Log("Testing: an unset named variable is refused by name, not read from the default")
	t.Setenv("FORGE_PERF_API_KEY", "")
	_, err = forgeAPIKey("forge-perf", &forge.Environment{APIKeyEnv: "FORGE_PERF_API_KEY"})
	require.ErrorContains(t, err, "FORGE_PERF_API_KEY is not set")
	require.ErrorContains(t, err, "forge-perf")
}
