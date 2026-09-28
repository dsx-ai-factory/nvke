// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// supportedK8sVersion is a release the fleet actually runs that understands
	// the anonymous-restricted AuthenticationConfiguration, so the golden
	// fixtures render the wiring the way production sees it.
	supportedK8sVersion = "v1.34.2"
	// unsupportedK8sVersion is an in-fleet release below the floor. 1.33 is the
	// sharpest case on purpose: AnonymousAuthConfigurableEndpoints is already
	// beta and on by default there, so the conditions themselves are fine --
	// what it cannot do is decode apiserver.config.k8s.io/v1
	// AuthenticationConfiguration, which only joins that group version in 1.34.
	unsupportedK8sVersion = "v1.33.6"
)

// updateGolden rewrites the testdata fixtures from the rendered output instead
// of asserting against them. The fixtures order map keys the way
// sigs.k8s.io/yaml emits them, so hand-editing them when a field is added is
// error-prone; run `go test ./internal/kubeadmconfig/... -update-golden` and
// review the diff instead.
var updateGolden = flag.Bool("update-golden", false, "rewrite testdata golden fixtures from rendered output")

func assertGolden(t *testing.T, goldenFile, got string) {
	t.Helper()

	path := filepath.Join("testdata", goldenFile)
	if *updateGolden {
		require.NoError(t, os.WriteFile(path, []byte(got), 0o600), "failed to update golden file %q", goldenFile)
		return
	}

	want, err := os.ReadFile(path)
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

// apiServerAuthConfigFixture is expected on every control-plane node now, not
// just the OIDC ones: the AuthenticationConfiguration carries the anonymous
// access restriction as well as any JWT providers.
func apiServerAuthConfigFixture(goldenFile string) fileFromFixture {
	return fileFromFixture{
		path:           "/etc/kubernetes/oidc/auth-config.yaml",
		goldenFilePath: goldenFile,
		mode:           0o600,
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
