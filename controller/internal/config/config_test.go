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

func TestUnit_ConfigValidateNodeAgentTLSJWT(t *testing.T) {
	t.Parallel()

	base := config.Config{
		MetricsListenAddress:    "127.0.0.1:9090",
		DBConnectionString:      "postgres://cwe:strong@localhost:5432/cwe?sslmode=disable",
		SpireServerAPIAddress:   "spire-server.spire-server.svc.cluster.local:8081",
		SpireJoinTokenTTL:       time.Hour,
		SupportedInfraProviders: []string{config.InfraProviderNico},
	}

	enabled := base
	enabled.SpiffeEnabled = true
	enabled.NodeAgentTLSJWTEnabled = true
	enabled.NodeAgentTLSJWTIssuer = "https://identity.example.test"
	enabled.NodeAgentTLSJWTJWKSURI = "https://identity.example.test/jwks"
	enabled.NodeAgentTLSJWTAlgorithms = []string{"RS256"}
	enabled.NodeAgentTLSJWTSubjectTrustDomain = "identity.example.test"
	enabled.SupportedInfraProviders = []string{config.InfraProviderInfra}

	missingIssuer := enabled
	missingIssuer.NodeAgentTLSJWTIssuer = ""
	missingJWKSURI := enabled
	missingJWKSURI.NodeAgentTLSJWTJWKSURI = ""
	missingAlgorithms := enabled
	missingAlgorithms.NodeAgentTLSJWTAlgorithms = nil
	missingSubjectTrustDomain := enabled
	missingSubjectTrustDomain.NodeAgentTLSJWTSubjectTrustDomain = ""
	missingSpiffe := enabled
	missingSpiffe.SpiffeEnabled = false
	nicoProvider := enabled
	nicoProvider.SupportedInfraProviders = []string{config.InfraProviderNico}
	bothProviders := enabled
	bothProviders.SupportedInfraProviders = []string{config.InfraProviderNico, config.InfraProviderInfra}

	type parameters struct {
		config  config.Config
		wantErr string
	}
	tests := map[string]parameters{
		"disabled needs no transport config": {
			config: base,
		},
		"enabled accepts controller JWT trust settings": {
			config: enabled,
		},
		"enabled rejects SPIFFE disabled": {
			config:  missingSpiffe,
			wantErr: "SPIFFE_ENABLED must be true when NODE_AGENT_TLS_JWT_ENABLED=true",
		},
		"enabled rejects a missing issuer": {
			config:  missingIssuer,
			wantErr: "NODE_AGENT_TLS_JWT_ISSUER is required when NODE_AGENT_TLS_JWT_ENABLED=true",
		},
		"enabled rejects a missing JWKS URI": {
			config:  missingJWKSURI,
			wantErr: "NODE_AGENT_TLS_JWT_JWKS_URI is required when NODE_AGENT_TLS_JWT_ENABLED=true",
		},
		"enabled rejects a missing algorithm allow-list": {
			config:  missingAlgorithms,
			wantErr: "NODE_AGENT_TLS_JWT_ALGORITHMS is required when NODE_AGENT_TLS_JWT_ENABLED=true",
		},
		"enabled rejects a missing subject trust domain": {
			config:  missingSubjectTrustDomain,
			wantErr: "NODE_AGENT_TLS_JWT_SUBJECT_TRUST_DOMAIN is required when NODE_AGENT_TLS_JWT_ENABLED=true",
		},
		"enabled accepts Nico": {
			config: nicoProvider,
		},
		"enabled accepts both providers": {
			config: bothProviders,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := params.config.Validate()
			if params.wantErr != "" {
				require.EqualError(t, err, params.wantErr)
				return
			}
			require.NoError(t, err)
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
