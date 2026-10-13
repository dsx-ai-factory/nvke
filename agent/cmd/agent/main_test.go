// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// The authorizer is the whole of this agent's access control, so these pin what it
// refuses as much as what it admits.
func TestAuthorizeClient(t *testing.T) {
	t.Parallel()

	const agentTrustDomain = "example.org"

	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{name: "renamed tenant path", id: "spiffe://example.org/tenant/nvke/service/cwe/bm/cwe"},
		{name: "renamed service path", id: "spiffe://example.org/nvke-service/cwe"},
		{name: "renamed prefix must end at slash", id: "spiffe://example.org/tenant/nvke/service-evil/cwe", wantErr: true},
		{name: "renamed caller in foreign trust domain", id: "spiffe://attacker.test/tenant/nvke/service/cwe", wantErr: true},
		{name: "legacy service path", id: "spiffe://example.org/nke-service/cloud-workflow-engine"},
		{name: "tenant-grammar path", id: "spiffe://example.org/tenant/nke/service/cloud-workflow-engine/bm/cwe"},
		{name: "tenant-grammar path, other service", id: "spiffe://example.org/tenant/nke/service/capi-manager/bm/cwe"},

		// Different paths that merely share leading characters.
		{name: "legacy prefix is not a substring match", id: "spiffe://example.org/nke-service-evil/cwe", wantErr: true},
		{name: "tenant prefix is not a substring match", id: "spiffe://example.org/tenant/nke/service-evil/cwe", wantErr: true},

		// The tenant segment is inside the prefix, so other tenants are refused.
		{name: "other tenant", id: "spiffe://example.org/tenant/acme/service/cloud-workflow-engine/bm/x", wantErr: true},

		{name: "unrelated path", id: "spiffe://example.org/nke-e2e/probe", wantErr: true},
		{name: "foreign trust domain", id: "spiffe://attacker.test/nke-service/cloud-workflow-engine", wantErr: true},
		{name: "trust domain is not a prefix match", id: "spiffe://example.org.attacker.test/nke-service/cwe", wantErr: true},
	}

	authorize := authorizeClient(agentTrustDomain)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			id, err := spiffeid.FromString(tc.id)
			if err != nil {
				t.Fatalf("parsing %q: %v", tc.id, err)
			}

			err = authorize(id)
			if tc.wantErr && err == nil {
				t.Fatalf("authorizeClient(%q) = nil, want an error", tc.id)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("authorizeClient(%q) = %v, want nil", tc.id, err)
			}
		})
	}
}

// A misconfigured authentication mode must stop the agent at startup. The
// alternative is a systemd unit that restarts forever while every handshake
// fails, which is what an unrecognized flag already produces.
func TestValidateAuthConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     AgentConfig
		wantErr string
	}{
		{
			name: "default mode needs no exchange configuration",
			cfg:  AgentConfig{GRPCAuthMode: authModeSPIFFEMTLS},
		},
		{
			name: "default mode tolerates disabled mTLS for development",
			cfg:  AgentConfig{GRPCAuthMode: authModeSPIFFEMTLS, DisableMTLS: true},
		},
		{
			name: "tls-jwt with an exchange endpoint",
			cfg: AgentConfig{
				GRPCAuthMode:           authModeTLSJWT,
				DSXIdentityExchangeURL: "https://identity.example.test/v1/exchange",
			},
		},
		{
			name: "tls-jwt cannot run on plaintext gRPC",
			cfg: AgentConfig{
				GRPCAuthMode:           authModeTLSJWT,
				DisableMTLS:            true,
				DSXIdentityExchangeURL: "https://identity.example.test/v1/exchange",
			},
			wantErr: "cannot be combined with --disable-mtls",
		},
		{
			name:    "tls-jwt without an exchange endpoint",
			cfg:     AgentConfig{GRPCAuthMode: authModeTLSJWT},
			wantErr: "requires --dsx-identity-exchange-url",
		},
		{
			name:    "unknown mode",
			cfg:     AgentConfig{GRPCAuthMode: "mtls"},
			wantErr: `--grpc-auth-mode must be "spiffe-mtls" or "tls-jwt", got "mtls"`,
		},
		{
			name:    "empty mode",
			cfg:     AgentConfig{},
			wantErr: `got ""`,
		},
		{
			name: "external mTLS alongside tls-jwt",
			cfg: AgentConfig{
				GRPCAuthMode:             authModeTLSJWT,
				DSXIdentityExchangeURL:   "https://identity.example.test/v1/exchange",
				ControllerClientCABundle: "/etc/nke/controller-ca.crt",
			},
		},
		{
			// The controller does not verify the certificate the agent serves
			// in this mode, so the identity token is the only thing that
			// authenticates the agent.
			name: "external mTLS without tls-jwt",
			cfg: AgentConfig{
				GRPCAuthMode:             authModeSPIFFEMTLS,
				ControllerClientCABundle: "/etc/nke/controller-ca.crt",
			},
			wantErr: "--controller-client-ca-bundle requires --grpc-auth-mode=tls-jwt",
		},
		{
			name: "external mTLS cannot run on plaintext gRPC",
			cfg: AgentConfig{
				GRPCAuthMode:             authModeTLSJWT,
				DisableMTLS:              true,
				DSXIdentityExchangeURL:   "https://identity.example.test/v1/exchange",
				ControllerClientCABundle: "/etc/nke/controller-ca.crt",
			},
			wantErr: "--controller-client-ca-bundle cannot be combined with --disable-mtls",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := validateAuthConfig(tc.cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateAuthConfig() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateAuthConfig() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateAuthConfig() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestControllerAudienceCompatibility(t *testing.T) {
	for _, audience := range []string{"", "nke-controller", "nvke-controller"} {
		if err := validateAuthConfig(AgentConfig{ControllerAudience: audience, GRPCAuthMode: authModeSPIFFEMTLS}); err != nil {
			t.Errorf("audience %q: %v", audience, err)
		}
	}
	if err := validateAuthConfig(AgentConfig{ControllerAudience: "other-service", GRPCAuthMode: authModeSPIFFEMTLS}); err == nil {
		t.Fatal("unsupported audience accepted")
	}
}
