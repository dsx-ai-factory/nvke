// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
)

// setMinimumEnv sets only what Config.Validate() already required, so these cases
// exercise the authentication fields and nothing else.
func setMinimumEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CNPG_SUPERUSER_PASSWORD", "not-the-dev-default")
}

// The tri-state selector. Unset AUTH_CONFIG_PATH parses as the disabled state.
func TestAuthEnvFields(t *testing.T) {
	tests := []struct {
		name           string
		env            map[string]string
		wantPath       string
		wantPermissive bool
	}{
		{
			name:           "both unset: authentication disabled",
			env:            nil,
			wantPath:       "",
			wantPermissive: false,
		},
		{
			// The two fields are independent env tags with no cross-field logic, so
			// the both-set case covers the singly-set ones. Whether a path with no
			// mode enforces is cmd/worker's decision, not this parser's.
			name: "path set, mode on: permissive",
			env: map[string]string{
				"AUTH_CONFIG_PATH": "/etc/nke/auth/authentication.yaml",
				"PERMISSIVE_MODE":  "true",
			},
			wantPath:       "/etc/nke/auth/authentication.yaml",
			wantPermissive: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setMinimumEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			cfg, err := config.ParseConfigFromEnv()
			require.NoError(t, err, "an unset AUTH_CONFIG_PATH must not stop NKE starting")
			assert.Equal(t, tt.wantPath, cfg.AuthConfigPath)
			assert.Equal(t, tt.wantPermissive, cfg.PermissiveMode)
		})
	}
}

// Validate ignores both authentication fields, set or unset. Requiring either
// would stop every deployment that has not enabled authentication.
func TestValidateIgnoresAuthFields(t *testing.T) {
	base := config.Config{
		MetricsListenAddress:            "127.0.0.1:9090",
		DBConnectionString:              "postgres://cwe:strong@localhost:5432/cwe?sslmode=disable",
		SpireServerAPIAddress:           "spire-server.spire-server.svc.cluster.local:8081",
		SpireJoinTokenTTL:               time.Hour,
		NodeAgentSpireAttestationMethod: config.SpireAttestationMethodTPM,
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
