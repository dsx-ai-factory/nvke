// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	_ "embed"
	"strings"
	"testing"

	apiserverv1 "k8s.io/apiserver/pkg/apis/apiserver/v1"
)

// renderedAuthConfig is what internal/kubeadmconfig renders for a cluster with
// the shared provider and one fleet-wide provider.
//
//go:embed testdata/auth-config.yaml
var renderedAuthConfig string

func exampleExpectation() ExpectedAuthentication {
	return ExpectedAuthentication{
		ClusterName: "example-cluster",
		Shared: &apiserverv1.Issuer{
			URL:          "https://nke.example.internal",
			DiscoveryURL: stringPointer("https://192.0.2.10:30443/.well-known/openid-configuration"),
		},
		Providers: []Provider{{
			Name: "example", IssuerURL: "https://idp.example.com", Audiences: []string{"example-audience"},
			UsernameClaim: "sub", GroupsClaim: "groups", RequiredClaims: map[string]string{"tenant": "example"},
			CABundle: "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
		}},
		ReservedUsers: []string{"spiffe://example.test/nke-service/nke-controller"},
	}
}

func TestCheckAuthenticationConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		config  string
		mutate  func(*ExpectedAuthentication)
		wantErr string
	}{
		{name: "the rendered shared and fleet-wide providers match", config: renderedAuthConfig},
		{
			name: "an unexpected shared provider fails the count", config: renderedAuthConfig,
			mutate:  func(want *ExpectedAuthentication) { want.Shared = nil },
			wantErr: "has 2 JWT authenticators, want 1",
		},
		{
			name: "the shared provider's audience is the cluster name", config: renderedAuthConfig,
			mutate:  func(want *ExpectedAuthentication) { want.ClusterName = "other-cluster" },
			wantErr: "shared provider: audiences",
		},
		{
			name: "a different shared issuer fails", config: renderedAuthConfig,
			mutate:  func(want *ExpectedAuthentication) { want.Shared.URL = "https://other.example.internal" },
			wantErr: "shared provider: issuer URL",
		},
		{
			name: "a different shared discovery URL fails", config: renderedAuthConfig,
			mutate:  func(want *ExpectedAuthentication) { want.Shared.DiscoveryURL = nil },
			wantErr: "shared provider: discovery URL",
		},
		{
			name: "a shared CA bundle the node does not carry fails", config: renderedAuthConfig,
			mutate:  func(want *ExpectedAuthentication) { want.Shared.CertificateAuthority = "PEM" },
			wantErr: "shared provider: certificate authority differs",
		},
		{
			name:    "a groups expression without a groups claim fails",
			config:  strings.Replace(renderedAuthConfig, "    groups:\n      claim: groups\n      prefix: \"\"\n", "    groups:\n      expression: claims.groups\n", 1),
			mutate:  func(want *ExpectedAuthentication) { want.Providers[0].GroupsClaim = "" },
			wantErr: `want no mapping`,
		},
		{
			name: "a different issuer fails", config: renderedAuthConfig,
			mutate:  func(want *ExpectedAuthentication) { want.Providers[0].IssuerURL = "https://other.example.com" },
			wantErr: `provider "example": issuer URL`,
		},
		{
			name: "a different CA bundle fails", config: renderedAuthConfig,
			mutate:  func(want *ExpectedAuthentication) { want.Providers[0].CABundle = "other" },
			wantErr: "certificate authority differs",
		},
		{
			name: "a missing username prefix fails", config: renderedAuthConfig,
			mutate:  func(want *ExpectedAuthentication) { want.Providers[0].UsernamePrefix = "example:" },
			wantErr: `username prefix is "", want "example:"`,
		},
		{
			name: "a missing required claim fails", config: renderedAuthConfig,
			mutate:  func(want *ExpectedAuthentication) { want.Providers[0].RequiredClaims["region"] = "west" },
			wantErr: "claim validation rules",
		},
		{
			name: "an unrefused reserved user fails", config: renderedAuthConfig,
			mutate: func(want *ExpectedAuthentication) {
				want.ReservedUsers = append(want.ReservedUsers, "spiffe://example.test/nke-service/other")
			},
			wantErr: `do not refuse "spiffe://example.test/nke-service/other"`,
		},
		{
			name: "a missing reserved prefix fails", config: strings.ReplaceAll(renderedAuthConfig, `"nvidia:"`, `"other:"`),
			wantErr: `do not refuse "nvidia:"`,
		},
		{
			name: "another kind fails", config: strings.Replace(renderedAuthConfig, "kind: AuthenticationConfiguration", "kind: Other", 1),
			wantErr: "document is apiserver.config.k8s.io/v1 Other",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			want := exampleExpectation()
			if tt.mutate != nil {
				tt.mutate(&want)
			}
			err := CheckAuthenticationConfig([]byte(tt.config), want)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckAuthenticationConfig() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("CheckAuthenticationConfig() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func stringPointer(value string) *string {
	return &value
}
