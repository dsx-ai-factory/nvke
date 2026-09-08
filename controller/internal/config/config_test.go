// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config_test

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	"github.com/NVIDIA/nke/controller/internal/config"
)

func TestConfig_ParseSupportedInfraProviders(t *testing.T) {
	type parameters struct {
		envValue *string
		want     []string
	}

	tests := map[string]parameters{
		"default": {
			want: []string{config.InfraProviderNico},
		},
		"override": {
			envValue: ptr.To("nico"),
			want:     []string{config.InfraProviderNico},
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			withEnv(t, "SUPPORTED_INFRA_PROVIDERS", params.envValue)
			setMinimumEnv(t)

			cfg, err := config.ParseConfigFromEnv()

			require.NoError(t, err)
			require.Equal(t, params.want, cfg.SupportedInfraProviders)
		})
	}
}

func TestConfig_ValidateSupportedInfraProviders(t *testing.T) {
	t.Parallel()

	base := config.Config{
		MetricsListenAddress:    "127.0.0.1:9090",
		DBConnectionString:      "postgres://cwe:strong@localhost:5432/cwe?sslmode=disable",
		SpireServerAPIAddress:   "spire-server.spire-server.svc.cluster.local:8081",
		SpireJoinTokenTTL:       time.Hour,
		SupportedInfraProviders: []string{config.InfraProviderNico, config.InfraProviderInfra},
		FWEAPIKey:               "test-key",
	}

	type parameters struct {
		providers []string
		want      []string
		wantErr   string
	}

	tests := map[string]parameters{
		"normalizes whitespace": {
			providers: []string{" nico ", " infra "},
			want:      []string{config.InfraProviderNico, config.InfraProviderInfra},
		},
		"zero value keeps binary default": {
			providers: nil,
			want:      []string{config.InfraProviderNico},
		},
		"rejects empty allowlist": {
			providers: []string{},
			wantErr:   "SUPPORTED_INFRA_PROVIDERS must contain at least one provider",
		},
		"rejects empty provider names": {
			providers: []string{"nico", " "},
			wantErr:   "SUPPORTED_INFRA_PROVIDERS must not contain empty provider names",
		},
		"rejects unknown providers": {
			providers: []string{"capi"},
			wantErr:   `SUPPORTED_INFRA_PROVIDERS contains unsupported provider "capi"`,
		},
		"rejects duplicates": {
			providers: []string{"nico", "nico"},
			wantErr:   `SUPPORTED_INFRA_PROVIDERS contains duplicate provider "nico"`,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := base
			cfg.SupportedInfraProviders = params.providers

			err := cfg.Validate()
			if params.wantErr != "" {
				require.EqualError(t, err, params.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, params.want, cfg.SupportedInfraProviders)
		})
	}
}

func withEnv(t *testing.T, name string, value *string) {
	t.Helper()

	oldValue, hadValue := os.LookupEnv(name)
	if value == nil {
		require.NoError(t, os.Unsetenv(name))
	} else {
		require.NoError(t, os.Setenv(name, *value))
	}
	t.Cleanup(func() {
		if hadValue {
			require.NoError(t, os.Setenv(name, oldValue))
			return
		}
		require.NoError(t, os.Unsetenv(name))
	})
}
