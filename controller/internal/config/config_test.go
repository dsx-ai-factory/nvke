// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config_test

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
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

func TestConfig_ParseSpireAttestationMethod(t *testing.T) {
	type parameters struct {
		envValue      *string
		spiffeEnabled *string
		want          config.SpireAttestationMethod
	}

	tests := map[string]parameters{
		"defaults to TPM": {
			want: config.SpireAttestationMethodTPM,
		},
		"accepts join token": {
			envValue:      ptr.To("joinToken"),
			spiffeEnabled: ptr.To("true"),
			want:          config.SpireAttestationMethodJoinToken,
		},
		"accepts DSX IMDS": {
			envValue:      ptr.To("dsxImds"),
			spiffeEnabled: ptr.To("true"),
			want:          config.SpireAttestationMethodDSXIMDS,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			withEnv(t, "NODE_AGENT_SPIRE_ATTESTATION_METHOD", params.envValue)
			withEnv(t, "SPIFFE_ENABLED", params.spiffeEnabled)
			if params.want == config.SpireAttestationMethodDSXIMDS {
				withEnv(t, "NODE_AGENT_DSX_IMDS_OPERATOR_TENANT", ptr.To("operator-1"))
			}
			setMinimumEnv(t)

			cfg, err := config.ParseConfigFromEnv()

			require.NoError(t, err)
			require.Equal(t, params.want, cfg.NodeAgent.SpireAttestationMethod)
		})
	}
}

func TestUnit_ParseDSXIMDSSettings(t *testing.T) {
	const checksum = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	type parameters struct {
		endpoint *string
		checksum *string
		wantURL  string
		wantSum  string
	}

	tests := map[string]parameters{
		"uses node image defaults": {
			wantURL: "http://169.254.169.254",
		},
		"accepts deployment overrides": {
			endpoint: ptr.To("https://metadata.example.test"),
			checksum: ptr.To(checksum),
			wantURL:  "https://metadata.example.test",
			wantSum:  checksum,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			withEnv(t, "NODE_AGENT_SPIRE_ATTESTATION_METHOD", ptr.To("dsxImds"))
			withEnv(t, "NODE_AGENT_DSX_IMDS_OPERATOR_TENANT", ptr.To("operator-1"))
			withEnv(t, "NODE_AGENT_DSX_IMDS_ENDPOINT", params.endpoint)
			withEnv(t, "NODE_AGENT_DSX_IMDS_PLUGIN_CHECKSUM", params.checksum)
			withEnv(t, "SPIFFE_ENABLED", ptr.To("true"))
			setMinimumEnv(t)

			cfg, err := config.ParseConfigFromEnv()

			require.NoError(t, err)
			require.Equal(t, params.wantURL, cfg.NodeAgent.DSXImds.Endpoint)
			require.Equal(t, params.wantSum, cfg.NodeAgent.DSXImds.PluginChecksum)
		})
	}
}

func TestUnit_ParseNodeAgentTLSJWTSettings(t *testing.T) {
	withEnv(t, "NODE_AGENT_TLS_JWT_ENABLED", ptr.To("true"))
	withEnv(t, "NODE_AGENT_TLS_JWT_ISSUER", ptr.To("https://identity.example.test"))
	withEnv(t, "NODE_AGENT_TLS_JWT_JWKS_URI", ptr.To("https://identity.example.test/jwks"))
	withEnv(t, "NODE_AGENT_TLS_JWT_ALGORITHMS", ptr.To("RS256,ES256"))
	withEnv(t, "NODE_AGENT_TLS_JWT_SUBJECT_TRUST_DOMAIN", ptr.To("identity.example.test"))
	withEnv(t, "NODE_AGENT_TLS_JWT_DSX_IDENTITY_EXCHANGE_URL", ptr.To("https://identity.example.test/v1/exchange"))
	withEnv(t, "SPIFFE_ENABLED", ptr.To("true"))
	withEnv(t, "SUPPORTED_INFRA_PROVIDERS", ptr.To(config.InfraProviderInfra))
	setMinimumEnv(t)

	cfg, err := config.ParseConfigFromEnv()

	require.NoError(t, err)
	require.True(t, cfg.NodeAgent.TLSJWT.Enabled)
	require.Equal(t, "https://identity.example.test", cfg.NodeAgent.TLSJWT.Issuer)
	require.Equal(t, "https://identity.example.test/jwks", cfg.NodeAgent.TLSJWT.JWKSURI)
	require.Equal(t, []string{"RS256", "ES256"}, cfg.NodeAgent.TLSJWT.Algorithms)
	require.Equal(t, "identity.example.test", cfg.NodeAgent.TLSJWT.SubjectTrustDomain)
	require.Equal(t, "https://identity.example.test/v1/exchange", cfg.NodeAgent.TLSJWT.DSXIdentityExchangeURL)
}

func TestConfig_ValidateSupportedInfraProviders(t *testing.T) {
	t.Parallel()

	base := config.Config{
		MetricsListenAddress:    "127.0.0.1:9090",
		DBConnectionString:      "postgres://cwe:strong@localhost:5432/cwe?sslmode=disable",
		SpireServerAPIAddress:   "spire-server.spire-server.svc.cluster.local:8081",
		SpireJoinTokenTTL:       time.Hour,
		SupportedInfraProviders: []string{config.InfraProviderNico, config.InfraProviderInfra},
		NodeAgent:               config.NodeAgentConfig{SpireAttestationMethod: config.SpireAttestationMethodTPM},
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
		NodeAgent:               config.NodeAgentConfig{SpireAttestationMethod: config.SpireAttestationMethodTPM},
	}

	enabled := base
	enabled.SpiffeEnabled = true
	enabled.NodeAgent.TLSJWT.Enabled = true
	enabled.NodeAgent.TLSJWT.Issuer = "https://identity.example.test"
	enabled.NodeAgent.TLSJWT.JWKSURI = "https://identity.example.test/jwks"
	enabled.NodeAgent.TLSJWT.Algorithms = []string{"RS256"}
	enabled.NodeAgent.TLSJWT.SubjectTrustDomain = "identity.example.test"
	enabled.NodeAgent.TLSJWT.DSXIdentityExchangeURL = "https://identity.example.test/v1/exchange"
	enabled.SupportedInfraProviders = []string{config.InfraProviderInfra}

	missingIssuer := enabled
	missingIssuer.NodeAgent.TLSJWT.Issuer = ""
	missingJWKSURI := enabled
	missingJWKSURI.NodeAgent.TLSJWT.JWKSURI = ""
	missingAlgorithms := enabled
	missingAlgorithms.NodeAgent.TLSJWT.Algorithms = nil
	missingSubjectTrustDomain := enabled
	missingSubjectTrustDomain.NodeAgent.TLSJWT.SubjectTrustDomain = ""
	missingExchangeURL := enabled
	missingExchangeURL.NodeAgent.TLSJWT.DSXIdentityExchangeURL = ""
	missingSpiffe := enabled
	missingSpiffe.SpiffeEnabled = false
	externalMTLS := missingSpiffe
	externalMTLS.NodeAgentExternalMTLSEnabled = true
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
		"enabled rejects all secure base transports disabled": {
			config:  missingSpiffe,
			wantErr: "SPIFFE_ENABLED or NODE_AGENT_EXTERNAL_MTLS_ENABLED must be true when NODE_AGENT_TLS_JWT_ENABLED=true",
		},
		"enabled accepts external mTLS without SPIFFE": {
			config: externalMTLS,
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
		"enabled rejects a missing DSX Identity exchange URL": {
			config:  missingExchangeURL,
			wantErr: "NODE_AGENT_TLS_JWT_DSX_IDENTITY_EXCHANGE_URL is required when NODE_AGENT_TLS_JWT_ENABLED=true",
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

func TestConfig_ValidateSpireAttestationMethod(t *testing.T) {
	t.Parallel()

	base := config.Config{
		MetricsListenAddress:    "127.0.0.1:9090",
		DBConnectionString:      "postgres://cwe:strong@localhost:5432/cwe?sslmode=disable",
		SpireServerAPIAddress:   "spire-server.spire-server.svc.cluster.local:8081",
		SpireJoinTokenTTL:       time.Hour,
		SupportedInfraProviders: []string{config.InfraProviderInfra},
		NodeAgent: config.NodeAgentConfig{
			SpireAttestationMethod: config.SpireAttestationMethodTPM,
			DSXImds: config.DSXImdsConfig{
				Endpoint:       "http://169.254.169.254",
				OperatorTenant: "operator-1",
			},
		},
	}

	type parameters struct {
		method                config.SpireAttestationMethod
		spiffeEnabled         bool
		dsxIMDSOperatorTenant *string
		dsxIMDSEndpoint       *string
		dsxIMDSPluginChecksum *string
		wantEndpoint          string
		wantErr               string
	}

	tests := map[string]parameters{
		"accepts TPM": {
			method: config.SpireAttestationMethodTPM,
		},
		"accepts join token": {
			method:        config.SpireAttestationMethodJoinToken,
			spiffeEnabled: true,
		},
		"rejects join token when SPIFFE is disabled": {
			method:  config.SpireAttestationMethodJoinToken,
			wantErr: "SPIFFE_ENABLED must be true when NODE_AGENT_SPIRE_ATTESTATION_METHOD is joinToken",
		},
		"accepts DSX IMDS when SPIFFE is enabled": {
			method:        config.SpireAttestationMethodDSXIMDS,
			spiffeEnabled: true,
			wantEndpoint:  "http://169.254.169.254",
		},
		"normalizes surrounding endpoint whitespace": {
			method:          config.SpireAttestationMethodDSXIMDS,
			spiffeEnabled:   true,
			dsxIMDSEndpoint: ptr.To("  https://metadata.example.test/base  "),
			wantEndpoint:    "https://metadata.example.test/base",
		},
		"accepts a configured checksum": {
			method:                config.SpireAttestationMethodDSXIMDS,
			spiffeEnabled:         true,
			dsxIMDSPluginChecksum: ptr.To("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		},
		"rejects DSX IMDS when SPIFFE is disabled": {
			method:  config.SpireAttestationMethodDSXIMDS,
			wantErr: "SPIFFE_ENABLED must be true when NODE_AGENT_SPIRE_ATTESTATION_METHOD is dsxImds",
		},
		"rejects DSX IMDS without an operator tenant": {
			method:                config.SpireAttestationMethodDSXIMDS,
			spiffeEnabled:         true,
			dsxIMDSOperatorTenant: ptr.To(""),
			wantErr:               "NODE_AGENT_DSX_IMDS_OPERATOR_TENANT is required when NODE_AGENT_SPIRE_ATTESTATION_METHOD is dsxImds",
		},
		"rejects an invalid DSX IMDS operator tenant": {
			method:                config.SpireAttestationMethodDSXIMDS,
			spiffeEnabled:         true,
			dsxIMDSOperatorTenant: ptr.To("operator/1"),
			wantErr:               "NODE_AGENT_DSX_IMDS_OPERATOR_TENANT must be a valid SPIFFE path segment: path segment characters are limited to letters, numbers, dots, dashes, and underscores",
		},
		"rejects DSX IMDS with an empty endpoint": {
			method:          config.SpireAttestationMethodDSXIMDS,
			spiffeEnabled:   true,
			dsxIMDSEndpoint: ptr.To(""),
			wantErr:         "NODE_AGENT_DSX_IMDS_ENDPOINT is required when NODE_AGENT_SPIRE_ATTESTATION_METHOD is dsxImds",
		},
		"rejects DSX IMDS with a malformed endpoint": {
			method:          config.SpireAttestationMethodDSXIMDS,
			spiffeEnabled:   true,
			dsxIMDSEndpoint: ptr.To("not-a-url"),
			wantErr:         `NODE_AGENT_DSX_IMDS_ENDPOINT must be an absolute HTTP(S) URL with a host, got "not-a-url"`,
		},
		"rejects DSX IMDS with a non-HTTP endpoint": {
			method:          config.SpireAttestationMethodDSXIMDS,
			spiffeEnabled:   true,
			dsxIMDSEndpoint: ptr.To("ftp://metadata.example.test"),
			wantErr:         `NODE_AGENT_DSX_IMDS_ENDPOINT must be an absolute HTTP(S) URL with a host, got "ftp://metadata.example.test"`,
		},
		"rejects an invalid checksum": {
			method:                config.SpireAttestationMethodDSXIMDS,
			spiffeEnabled:         true,
			dsxIMDSPluginChecksum: ptr.To("abc123"),
			wantErr:               "NODE_AGENT_DSX_IMDS_PLUGIN_CHECKSUM must be a 64-character SHA-256 hex digest",
		},
		"rejects an empty method": {
			wantErr: `NODE_AGENT_SPIRE_ATTESTATION_METHOD must be one of tpm, joinToken, or dsxImds, got ""`,
		},
		"rejects an unknown method": {
			method:  "unknown",
			wantErr: `NODE_AGENT_SPIRE_ATTESTATION_METHOD must be one of tpm, joinToken, or dsxImds, got "unknown"`,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := base
			cfg.NodeAgent.SpireAttestationMethod = params.method
			cfg.SpiffeEnabled = params.spiffeEnabled
			if params.dsxIMDSOperatorTenant != nil {
				cfg.NodeAgent.DSXImds.OperatorTenant = *params.dsxIMDSOperatorTenant
			}
			if params.dsxIMDSEndpoint != nil {
				cfg.NodeAgent.DSXImds.Endpoint = *params.dsxIMDSEndpoint
			}
			if params.dsxIMDSPluginChecksum != nil {
				cfg.NodeAgent.DSXImds.PluginChecksum = *params.dsxIMDSPluginChecksum
			}

			err := cfg.Validate()
			if params.wantErr != "" {
				require.EqualError(t, err, params.wantErr)
				return
			}
			require.NoError(t, err)
			if params.wantEndpoint != "" {
				require.Equal(t, params.wantEndpoint, cfg.NodeAgent.DSXImds.Endpoint)
			}
		})
	}
}

func TestUnit_ConfigParseNodeAgentExternalMTLS(t *testing.T) {
	withEnv(t, "NODE_AGENT_EXTERNAL_MTLS_ENABLED", ptr.To("true"))
	setMinimumEnv(t)

	cfg, err := config.ParseConfigFromEnv()

	require.NoError(t, err)
	require.True(t, cfg.NodeAgentExternalMTLSEnabled)
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
