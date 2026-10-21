// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
)

// setMinimumEnv sets only what ParseConfigFromEnv and Config.Validate require,
// so these cases exercise the authentication fields and nothing else.
func setMinimumEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CNPG_SUPERUSER_PASSWORD", "not-the-dev-default")
	t.Setenv("CONTROLLER_CONFIG_PATH", "/etc/nvke/controller-config/config.yaml")
}

// Unset AUTH_CONFIG_PATH parses as the disabled state. Permissive mode comes
// from nke-controller-config.
func TestAuthEnvFields(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		wantPath string
	}{
		{
			name:     "unset: authentication disabled",
			env:      nil,
			wantPath: "",
		},
		{
			name:     "set",
			env:      map[string]string{"AUTH_CONFIG_PATH": "/etc/nke/auth/authentication.yaml"},
			wantPath: "/etc/nke/auth/authentication.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setMinimumEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			cfg, err := config.ParseConfigFromEnv()
			require.NoError(t, err, "an unset AUTH_CONFIG_PATH must not stop NVKE starting")
			assert.Equal(t, tt.wantPath, cfg.AuthConfigPath)
		})
	}
}

// Validate ignores both authentication fields, set or unset. Requiring either
// would stop every deployment that has not enabled authentication.
func TestValidateIgnoresAuthFields(t *testing.T) {
	base := config.Config{
		MetricsListenAddress:  "127.0.0.1:9090",
		DBConnectionString:    "postgres://cwe:strong@localhost:5432/cwe?sslmode=disable",
		SpireServerAPIAddress: "spire-server.spire-server.svc.cluster.local:8081",
		SpireJoinTokenTTL:     time.Hour,
		NodeAgent:             config.NodeAgentConfig{SpireAttestationMethod: config.SpireAttestationMethodTPM},
	}

	// Unset is the disabled state every existing deployment upgrades into. This
	// is the case a new required-auth check would break.
	require.NoError(t, base.Validate(), "an unset AUTH_CONFIG_PATH must still validate")

	// And set changes nothing, in either mode: Validate has no opinion either way.
	for _, permissive := range []bool{false, true} {
		configured := base
		configured.AuthConfigPath = "/etc/nke/auth/auth.yaml"
		configured.PermissiveMode = permissive
		require.NoError(t, configured.Validate(), "permissive=%t must not change validation", permissive)
	}
}
