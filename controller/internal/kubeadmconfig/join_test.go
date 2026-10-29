// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
)

func TestJoinConfigRender_Golden(t *testing.T) {
	tests := []struct {
		name             string
		config           func() *JoinConfig
		wantConfig       string
		wantFiles        []fileFromFixture
		wantKubeletPatch string
	}{
		{
			name:       "worker join config with provisioning taint",
			wantConfig: "join_worker.yaml",
			config:     validWorkerJoinConfig,
		},
		{
			name:       "worker join config renders kubeadm v1beta4 for a supported Kubernetes version",
			wantConfig: "join_worker_v1beta4.yaml",
			config: func() *JoinConfig {
				cfg := validWorkerJoinConfig()
				cfg.KubernetesVersion = supportedK8sVersion
				return cfg
			},
		},
		{
			name:       "control-plane join config without encryption",
			wantConfig: "join_control_plane.yaml",
			config:     validControlPlaneJoinConfig,
			wantFiles: []fileFromFixture{
				apiServerTerminationGracePatchFixture(),
				apiServerAuditPolicyFixture(),
				apiServerAuthConfigFixture("auth_config_anonymous_only.yaml"),
			},
		},
		{
			name:       "control-plane join includes auth prerequisite file",
			wantConfig: "join_control_plane.yaml",
			config: func() *JoinConfig {
				cfg := validControlPlaneJoinConfig()
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
			name:       "control-plane join with multi-provider auth prerequisite file",
			wantConfig: "join_control_plane.yaml",
			config: func() *JoinConfig {
				cfg := validControlPlaneJoinConfig()
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
			// Workers never receive the auth-config file because kube-apiserver
			// only runs on control-plane nodes.
			name:       "worker join omits auth prerequisite file",
			wantConfig: "join_worker.yaml",
			config: func() *JoinConfig {
				cfg := validWorkerJoinConfig()
				cfg.AuthenticationConfig = testAuthenticationConfig()
				return cfg
			},
		},
		{
			name:             "worker join with custom system reserved memory",
			wantConfig:       "join_worker.yaml",
			wantKubeletPatch: "kubelet_configuration_patch_32gi.yaml",
			config: func() *JoinConfig {
				cfg := validWorkerJoinConfig()
				cfg.SystemReservedMemory = "32Gi"
				return cfg
			},
		},
		{
			name:             "worker join with custom kube reserved memory",
			wantConfig:       "join_worker.yaml",
			wantKubeletPatch: "kubelet_configuration_patch_250mi.yaml",
			config: func() *JoinConfig {
				cfg := validWorkerJoinConfig()
				cfg.KubeReservedMemory = "250Mi"
				return cfg
			},
		},
		{
			name:       "worker join config with local-dev preflight skip",
			wantConfig: "join_local_dev.yaml",
			config: func() *JoinConfig {
				cfg := validWorkerJoinConfig()
				cfg.KubeadmContainerMode = true
				return cfg
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.config()
			artifacts, err := cfg.Render()
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

func TestJoinConfigRenderReturnsEmptyAuthenticationConfigError(t *testing.T) {
	cfg := validControlPlaneJoinConfig()
	cfg.AuthenticationConfig = &OIDCAuthenticationConfig{}

	_, err := cfg.Render()

	require.ErrorContains(t, err, "building AuthenticationConfig file")
	require.ErrorContains(t, err, "rendering AuthenticationConfiguration requires at least one JWT provider")
}

func TestNewJoinConfigFromString(t *testing.T) {
	tests := []struct {
		name       string
		nodeIP     string
		nodeName   string
		joinString string
		want       *JoinConfig
		wantErr    error
	}{
		{
			name:       "valid worker join string",
			nodeIP:     "10.0.0.1",
			nodeName:   "worker-1",
			joinString: "kubeadm join 1.2.3.4:6443 --token abcdef --discovery-token-ca-cert-hash sha256:123456",
			want: &JoinConfig{
				APIServerEndpoint:        "1.2.3.4:6443",
				Token:                    "abcdef",
				DiscoveryTokenCACertHash: "sha256:123456",
				NodeName:                 "worker-1",
				NodeIP:                   "10.0.0.1",
				NodeTaints: []constants.NodeTaint{
					constants.NodeTaintProvisioning(),
				},
			},
		},
		{
			name:       "valid worker join string without endpoint port",
			nodeIP:     "10.0.0.1",
			nodeName:   "worker-1",
			joinString: "kubeadm join api.example.com --token abcdef --discovery-token-ca-cert-hash sha256:123456",
			want: &JoinConfig{
				APIServerEndpoint:        "api.example.com",
				Token:                    "abcdef",
				DiscoveryTokenCACertHash: "sha256:123456",
				NodeName:                 "worker-1",
				NodeIP:                   "10.0.0.1",
				NodeTaints: []constants.NodeTaint{
					constants.NodeTaintProvisioning(),
				},
			},
		},
		{
			name:       "valid control-plane join string",
			nodeIP:     "10.0.0.2",
			nodeName:   "cp-1",
			joinString: "kubeadm join 1.2.3.4:6443 --control-plane --certificate-key certkey123 --token abcdef --discovery-token-ca-cert-hash sha256:123456",
			want: &JoinConfig{
				ControlPlane:             true,
				NodeIP:                   "10.0.0.2",
				CertificateKey:           "certkey123",
				APIServerEndpoint:        "1.2.3.4:6443",
				NodeName:                 "cp-1",
				DiscoveryTokenCACertHash: "sha256:123456",
				Token:                    "abcdef",
			},
		},
		{
			name:       "invalid prefix",
			nodeIP:     "10.0.0.1",
			nodeName:   "worker-1",
			joinString: "join 1.2.3.4:6443 --token abcdef --discovery-token-ca-cert-hash sha256:123456",
			wantErr:    ErrInvalidJoinString,
		},
		{
			name:       "missing api server endpoint",
			nodeIP:     "10.0.0.1",
			nodeName:   "worker-1",
			joinString: "kubeadm join --token abcdef --discovery-token-ca-cert-hash sha256:123456",
			wantErr:    ErrMissingAPIServerEndpoint,
		},
		{
			name:       "missing token",
			nodeIP:     "10.0.0.1",
			nodeName:   "worker-1",
			joinString: "kubeadm join 1.2.3.4:6443 --discovery-token-ca-cert-hash sha256:123456",
			wantErr:    ErrMissingToken,
		},
		{
			name:       "missing discovery hash",
			nodeIP:     "10.0.0.1",
			nodeName:   "worker-1",
			joinString: "kubeadm join 1.2.3.4:6443 --token abcdef",
			wantErr:    ErrMissingHash,
		},
		{
			name:       "missing certificate key for control-plane join",
			nodeIP:     "10.0.0.2",
			nodeName:   "cp-1",
			joinString: "kubeadm join 1.2.3.4:6443 --control-plane --token abcdef --discovery-token-ca-cert-hash sha256:123456",
			wantErr:    ErrMissingCertificateKey,
		},
		{
			name:       "missing node name",
			nodeIP:     "10.0.0.1",
			nodeName:   "",
			joinString: "kubeadm join 1.2.3.4:6443 --token abcdef --discovery-token-ca-cert-hash sha256:123456",
			wantErr:    ErrMissingNodeName,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewJoinConfigFromString(tt.nodeIP, tt.nodeName, tt.joinString)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestJoinConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*JoinConfig)
		wantErr string
	}{
		{
			name: "missing control plane endpoint",
			mutate: func(cfg *JoinConfig) {
				cfg.APIServerEndpoint = ""
			},
			wantErr: "control plane endpoint is required",
		},
		{
			name: "missing node name",
			mutate: func(cfg *JoinConfig) {
				cfg.NodeName = ""
			},
			wantErr: "node name is required",
		},
		{
			name: "missing discovery hash",
			mutate: func(cfg *JoinConfig) {
				cfg.DiscoveryTokenCACertHash = ""
			},
			wantErr: "discovery token CA cert hash is required",
		},
		{
			name: "missing token",
			mutate: func(cfg *JoinConfig) {
				cfg.Token = ""
			},
			wantErr: "token is required",
		},
		{
			name: "control-plane join missing advertise address",
			mutate: func(cfg *JoinConfig) {
				cfg.ControlPlane = true
				cfg.NodeIP = ""
				cfg.CertificateKey = "certkey123"
				cfg.NodeTaints = nil
			},
			wantErr: "advertise address is required for control plane nodes",
		},
		{
			name: "control-plane join missing certificate key",
			mutate: func(cfg *JoinConfig) {
				cfg.ControlPlane = true
				cfg.NodeIP = "10.0.0.2"
				cfg.CertificateKey = ""
				cfg.NodeTaints = nil
			},
			wantErr: "certificate key is required for control plane nodes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validWorkerJoinConfig()
			tt.mutate(cfg)

			err := cfg.Validate()
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func validWorkerJoinConfig() *JoinConfig {
	return &JoinConfig{
		APIServerEndpoint:        "1.2.3.4:6443",
		Token:                    "token123",
		DiscoveryTokenCACertHash: "sha256:abc123",
		NodeName:                 "worker-1",
		NodeIP:                   "10.0.0.3",
		NodeTaints: []constants.NodeTaint{
			constants.NodeTaintProvisioning(),
		},
	}
}

func validControlPlaneJoinConfig() *JoinConfig {
	cfg := validWorkerJoinConfig()
	cfg.KubernetesVersion = supportedK8sVersion
	cfg.NodeName = "cp-1"
	cfg.NodeIP = "10.0.0.2"
	cfg.ControlPlane = true
	cfg.CertificateKey = "certkey123"
	cfg.NodeTaints = nil
	return cfg
}
