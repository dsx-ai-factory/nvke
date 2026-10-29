// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	initBootstrapTokenLineRE  = regexp.MustCompile(`(?m)^(\s*token:\s*"?)([0-9a-f]{6}\.[0-9a-f]{16})("?\s*)$`)
	initBootstrapTokenValueRE = regexp.MustCompile(`^[0-9a-f]{6}\.[0-9a-f]{16}$`)
)

func TestInitConfigRender_Golden(t *testing.T) {
	tests := []struct {
		name             string
		config           func() *InitConfig
		wantConfig       string
		wantFiles        []fileFromFixture
		wantKubeletPatch string
	}{
		{
			name:       "base init config with explicit token",
			wantConfig: "init_base.yaml",
			config:     validInitConfig,
			wantFiles: []fileFromFixture{
				apiServerTerminationGracePatchFixture(),
				apiServerAuditPolicyFixture(),
				apiServerAuthConfigFixture("auth_config_anonymous_only.yaml"),
			},
		},
		{
			name:       "init config with encryption enabled",
			wantConfig: "init_encryption.yaml",
			config: func() *InitConfig {
				cfg := validInitConfig()
				cfg.EnableEncryption = true
				return cfg
			},
			wantFiles: []fileFromFixture{
				apiServerTerminationGracePatchFixture(),
				apiServerAuditPolicyFixture(),
				apiServerAuthConfigFixture("auth_config_anonymous_only.yaml"),
			},
		},
		{
			name:       "init config with kube-proxy skipped",
			wantConfig: "init_skip_kube_proxy.yaml",
			config: func() *InitConfig {
				cfg := validInitConfig()
				cfg.SkipKubeProxy = true
				return cfg
			},
			wantFiles: []fileFromFixture{
				apiServerTerminationGracePatchFixture(),
				apiServerAuditPolicyFixture(),
				apiServerAuthConfigFixture("auth_config_anonymous_only.yaml"),
			},
		},
		{
			name:       "init config with local-dev kube-proxy tuning",
			wantConfig: "init_local_dev.yaml",
			config: func() *InitConfig {
				cfg := validInitConfig()
				cfg.KubeadmContainerMode = true
				return cfg
			},
			wantFiles: []fileFromFixture{
				apiServerTerminationGracePatchFixture(),
				apiServerAuditPolicyFixture(),
				apiServerAuthConfigFixture("auth_config_anonymous_only.yaml"),
			},
		},
		{
			name:       "init config with custom image repository",
			wantConfig: "init_custom_image_repository.yaml",
			config: func() *InitConfig {
				cfg := validInitConfig()
				cfg.ImageRepository = "my-registry.example.com/k8s"
				return cfg
			},
			wantFiles: []fileFromFixture{
				apiServerTerminationGracePatchFixture(),
				apiServerAuditPolicyFixture(),
				apiServerAuthConfigFixture("auth_config_anonymous_only.yaml"),
			},
		},
		{
			name:             "init config with custom system reserved memory",
			wantConfig:       "init_base.yaml",
			wantKubeletPatch: "kubelet_configuration_patch_32gi.yaml",
			config: func() *InitConfig {
				cfg := validInitConfig()
				cfg.SystemReservedMemory = "32Gi"
				return cfg
			},
			wantFiles: []fileFromFixture{
				apiServerTerminationGracePatchFixture(),
				apiServerAuditPolicyFixture(),
				apiServerAuthConfigFixture("auth_config_anonymous_only.yaml"),
			},
		},
		{
			name:             "init config with custom kube reserved memory",
			wantConfig:       "init_base.yaml",
			wantKubeletPatch: "kubelet_configuration_patch_250mi.yaml",
			config: func() *InitConfig {
				cfg := validInitConfig()
				cfg.KubeReservedMemory = "250Mi"
				return cfg
			},
			wantFiles: []fileFromFixture{
				apiServerTerminationGracePatchFixture(),
				apiServerAuditPolicyFixture(),
				apiServerAuthConfigFixture("auth_config_anonymous_only.yaml"),
			},
		},
		{
			name:       "init config with auth prerequisite file",
			wantConfig: "init_auth.yaml",
			config: func() *InitConfig {
				cfg := validInitConfig()
				cfg.AuthenticationConfig = testAuthenticationConfig()
				return cfg
			},
			wantFiles: []fileFromFixture{
				apiServerTerminationGracePatchFixture(),
				apiServerAuditPolicyFixture(),
				apiServerAuthConfigFixture("auth_config_simple.yaml"),
			},
		},
		{
			name:       "init config with multi-provider auth prerequisite file",
			wantConfig: "init_auth.yaml",
			config: func() *InitConfig {
				cfg := validInitConfig()
				cfg.AuthenticationConfig = testMultiProviderAuthenticationConfig()
				return cfg
			},
			wantFiles: []fileFromFixture{
				apiServerTerminationGracePatchFixture(),
				apiServerAuditPolicyFixture(),
				apiServerAuthConfigFixture("auth_config_multi_provider.yaml"),
			},
		},
		{
			name:       "init config with encryption and auth prerequisite file",
			wantConfig: "init_encryption_auth.yaml",
			config: func() *InitConfig {
				cfg := validInitConfig()
				cfg.EnableEncryption = true
				cfg.AuthenticationConfig = testAuthenticationConfig()
				return cfg
			},
			wantFiles: []fileFromFixture{
				apiServerTerminationGracePatchFixture(),
				apiServerAuditPolicyFixture(),
				apiServerAuthConfigFixture("auth_config_simple.yaml"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			artifacts, err := tt.config().Render()
			require.NoError(t, err)

			assertGolden(t, tt.wantConfig, string(artifacts.ConfigYAML))
			kubeletPatch := tt.wantKubeletPatch
			if kubeletPatch == "" {
				kubeletPatch = "kubelet_configuration_patch.yaml"
			}
			wantFiles := append([]fileFromFixture{kubeletConfigurationPatchFixture(kubeletPatch)}, tt.wantFiles...)
			require.Len(t, artifacts.Files, len(wantFiles))
			for i, want := range wantFiles {
				got := artifacts.Files[i]
				assert.Equal(t, want.path, got.Path)
				assert.Equal(t, want.mode, got.Mode)
				assert.Equal(t, want.owner, got.Owner)
				assertGolden(t, want.goldenFilePath, string(got.Content))
			}
		})
	}
}

func TestInitConfigRenderGeneratesMissingToken(t *testing.T) {
	cfg := validInitConfig()
	cfg.Token = ""

	artifacts, err := cfg.Render()
	require.NoError(t, err)
	got := string(artifacts.ConfigYAML)

	match := initBootstrapTokenLineRE.FindStringSubmatch(got)
	matchIndices := initBootstrapTokenLineRE.FindStringSubmatchIndex(got)
	require.Len(t, match, 4, "rendered bootstrap token line")
	require.Len(t, matchIndices, 8, "rendered bootstrap token line indices")

	generatedToken := match[2]
	require.Regexp(t, initBootstrapTokenValueRE, generatedToken, "generated token should match kubeadm token format")
	assert.Empty(t, cfg.Token, "config token should be empty")

	normalized := got[:matchIndices[4]] + "abcdef.0123456789abcdef" + got[matchIndices[5]:]
	assertGolden(t, "init_base.yaml", normalized)
	require.Len(t, artifacts.Files, 4)
	assert.Equal(t, kubeletConfigurationPatchFixture("kubelet_configuration_patch.yaml").path, artifacts.Files[0].Path)
	assertGolden(t, "kubelet_configuration_patch.yaml", string(artifacts.Files[0].Content))
	assert.Equal(t, apiServerTerminationGracePatchFixture().path, artifacts.Files[1].Path)
	assertGolden(t, apiServerTerminationGracePatchFixture().goldenFilePath, string(artifacts.Files[1].Content))
	assert.Equal(t, apiServerAuditPolicyFixture().path, artifacts.Files[2].Path)
	assertGolden(t, apiServerAuditPolicyFixture().goldenFilePath, string(artifacts.Files[2].Content))
	authConfig := apiServerAuthConfigFixture("auth_config_anonymous_only.yaml")
	assert.Equal(t, authConfig.path, artifacts.Files[3].Path)
	assertGolden(t, authConfig.goldenFilePath, string(artifacts.Files[3].Content))
}

func TestInitConfigRenderReturnsInvalidTokenError(t *testing.T) {
	cfg := validInitConfig()
	cfg.Token = "not-a-token"

	_, err := cfg.Render()

	require.ErrorContains(t, err, "invalid bootstrap token")
	require.ErrorContains(t, err, "not-a-token")
}

func TestInitConfigRenderReturnsEmptyAuthenticationConfigError(t *testing.T) {
	cfg := validInitConfig()
	cfg.AuthenticationConfig = &OIDCAuthenticationConfig{}

	_, err := cfg.Render()

	require.ErrorContains(t, err, "building AuthenticationConfig file")
	require.ErrorContains(t, err, "rendering AuthenticationConfiguration requires at least one JWT provider")
}

func TestInitConfigRenderDefaultsEmptyEtcdQuotaBackendBytes(t *testing.T) {
	cfg := validInitConfig()
	cfg.EtcdQuotaBackendBytes = ""

	artifacts, err := cfg.Render()
	require.NoError(t, err)
	assert.Contains(t, string(artifacts.ConfigYAML), "- name: quota-backend-bytes\n      value: \""+EtcdQuotaBackendBytes+"\"")
	assert.Empty(t, cfg.EtcdQuotaBackendBytes, "Render must not mutate the caller's config")
}

func TestRenderAuthenticationConfigNilConfig(t *testing.T) {
	_, err := renderAuthenticationConfig(nil, supportedK8sVersion)
	require.ErrorContains(t, err, "non-nil config")
}

func TestInitConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*InitConfig)
		wantErr string
	}{
		{
			name: "missing advertise address",
			mutate: func(cfg *InitConfig) {
				cfg.NodeIP = ""
			},
			wantErr: "advertise address is required",
		},
		{
			name: "missing control plane endpoint",
			mutate: func(cfg *InitConfig) {
				cfg.APIServerEndpoint = ""
			},
			wantErr: "control plane endpoint is required",
		},
		{
			name: "missing node name",
			mutate: func(cfg *InitConfig) {
				cfg.NodeName = ""
			},
			wantErr: "node name is required",
		},
		{
			name: "missing kubernetes version",
			mutate: func(cfg *InitConfig) {
				cfg.KubernetesVersion = ""
			},
			wantErr: "kubernetes version is required",
		},
		{
			name: "missing pod subnet",
			mutate: func(cfg *InitConfig) {
				cfg.PodSubnet = ""
			},
			wantErr: "pod subnet is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validInitConfig()
			tt.mutate(cfg)

			err := cfg.Validate()
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestKubeadmPodSubnet(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "single CIDR",
			in:   "10.244.0.0/16",
			want: "10.244.0.0/16",
		},
		{
			name: "multiple IPv4 CIDRs",
			in:   "10.240.0.0/16, 10.241.0.0/16",
			want: "10.240.0.0/16",
		},
		{
			name: "dual-stack keeps one CIDR per family",
			in:   "10.240.0.0/16, fd00:10:240::/56, 10.241.0.0/16, fd00:10:241::/56",
			want: "10.240.0.0/16,fd00:10:240::/56",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, kubeadmPodSubnet(tt.in))
		})
	}
}

func validInitConfig() *InitConfig {
	return &InitConfig{
		NodeIP:                "192.168.1.10",
		APIServerEndpoint:     "cluster.local:6443",
		NodeName:              "node1",
		KubernetesVersion:     supportedK8sVersion,
		PodSubnet:             "10.244.0.0/16",
		ServiceSubnet:         "10.96.0.0/12",
		EtcdQuotaBackendBytes: EtcdQuotaBackendBytes,
		Token:                 "abcdef.0123456789abcdef",
	}
}
