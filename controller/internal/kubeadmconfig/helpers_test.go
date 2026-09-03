// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertGolden(t *testing.T, goldenFile, got string) {
	t.Helper()

	want, err := os.ReadFile(filepath.Join("testdata", goldenFile))
	require.NoError(t, err, "failed to read golden file %q", goldenFile)
	assert.Equal(t, string(want), got, "golden mismatch for %s", goldenFile)
}

// fileFromFixture describes an expected rendered file in a kubeadm config
// table test. The content is asserted against a golden fixture to keep the
// expected payload alongside the other rendered artifacts.
type fileFromFixture struct {
	path           string
	goldenFilePath string
	mode           uint32
	owner          string
}

func apiServerTerminationGracePatchFixture() fileFromFixture {
	return fileFromFixture{
		path:           "/etc/kubernetes/patches/kube-apiserver-base-graceful-shutdown+merge.yaml",
		goldenFilePath: "kube_apiserver_termination_grace_patch.yaml",
		mode:           0o644,
		owner:          "root:root",
	}
}

func apiServerAuditPolicyFixture() fileFromFixture {
	return fileFromFixture{
		path:           "/etc/kubernetes/policies/audit-policy.yaml",
		goldenFilePath: "audit_policy.yaml",
		mode:           0o644,
		owner:          "root:root",
	}
}

func kubeletConfigurationPatchFixture(goldenFile string) fileFromFixture {
	return fileFromFixture{
		path:           "/etc/kubernetes/patches/kubeletconfiguration0+merge.yaml",
		goldenFilePath: goldenFile,
		mode:           0o644,
		owner:          "root:root",
	}
}

func testAuthenticationConfig() *OIDCAuthenticationConfig {
	return &OIDCAuthenticationConfig{
		JWT: []OIDCProvider{
			{
				Issuer: OIDCIssuer{
					URL:                  "https://nke.site-controller.internal",
					DiscoveryURL:         stringPtr("https://10.0.0.10:30443/.well-known/openid-configuration"),
					CertificateAuthority: "-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----",
					Audiences:            []string{"tenant-cluster-42"},
				},
				ClaimMappings: OIDCClaimMappings{
					Username: OIDCPrefixedClaimOrExpression{
						Claim:  "sub",
						Prefix: stringPtr(""),
					},
				},
			},
		},
	}
}

func testMultiProviderAuthenticationConfig() *OIDCAuthenticationConfig {
	opsPrefix := "ops:"

	return &OIDCAuthenticationConfig{
		JWT: []OIDCProvider{
			{
				Issuer: OIDCIssuer{
					URL:                  "https://issuer-one.example.com",
					DiscoveryURL:         stringPtr("https://10.0.0.10:30443/.well-known/openid-configuration"),
					CertificateAuthority: "-----BEGIN CERTIFICATE-----\nissuer-one\n-----END CERTIFICATE-----\n",
					Audiences:            []string{"cluster-a", "cluster-a-alt"},
					AudienceMatchPolicy:  "MatchAny",
				},
				ClaimMappings: OIDCClaimMappings{
					Username: OIDCPrefixedClaimOrExpression{
						Claim:  "sub",
						Prefix: stringPtr(""),
					},
					Groups: OIDCPrefixedClaimOrExpression{
						Claim:  "groups",
						Prefix: &opsPrefix,
					},
				},
				ClaimValidationRules: []OIDCClaimValidationRule{
					{
						Expression: "claims.hd == 'example.com'",
						Message:    "hosted domain must be example.com",
					},
				},
				UserValidationRules: []OIDCUserValidationRule{
					{
						Expression: "!user.username.startsWith('system:')",
						Message:    "system usernames are reserved",
					},
				},
			},
			{
				Issuer: OIDCIssuer{
					URL:                  "https://issuer-two.example.com",
					CertificateAuthority: "-----BEGIN CERTIFICATE-----\nissuer-two\n-----END CERTIFICATE-----\n",
					Audiences:            []string{"cluster-b"},
				},
				ClaimMappings: OIDCClaimMappings{
					Username: OIDCPrefixedClaimOrExpression{
						Expression: "claims.email",
					},
				},
			},
		},
	}
}

func stringPtr(value string) *string {
	return &value
}
