// SPDX-FileCopyrightText: Copyright (c) 2024 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:gocyclo,cyclop // The complexity is acceptable for this file.
package kubeadmconfig

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	kubeadmv1beta3 "github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig/thirdparty/kubeadmtypes/v1beta3"
)

var (
	ErrMissingNodeName          = errors.New("missing node name")
	ErrInvalidJoinString        = errors.New("invalid join string format")
	ErrMissingAPIServerEndpoint = errors.New("missing API server endpoint in join string")
	ErrMissingToken             = errors.New("missing token in join string")
	ErrMissingHash              = errors.New("missing discovery token CA cert hash in join string")
	ErrMissingCertificateKey    = errors.New("missing certificate key in join string")
)

// JoinConfig represents the configuration for kubeadm join.
type JoinConfig struct {
	// ControlPlane indicates whether the node is a control plane node.
	ControlPlane bool
	// NodeIP is required for joining control plane nodes.
	NodeIP string
	// CertificateKey is required for joining control plane nodes.
	CertificateKey           string
	APIServerEndpoint        string
	NodeName                 string
	DiscoveryTokenCACertHash string
	Token                    string
	// NodeTaints specify the list of taints that will be added to joining nodes
	NodeTaints []constants.NodeTaint
	// AuthenticationConfig configures the authentication-config flag in the
	// api server and generates the proper AuthenticationConfig file for OIDC.
	AuthenticationConfig *OIDCAuthenticationConfig
	// KubeadmContainerMode enables workarounds for kubeadm running inside a
	// container-backed node, such as CAPD DockerMachine nodes.
	KubeadmContainerMode bool
	// SystemReservedMemory is the memory reserved for system processes by the
	// kubelet configuration patch. It defaults to 16Gi when empty.
	SystemReservedMemory string
	// KubernetesVersion is the joining cluster's Kubernetes version. It decides
	// whether the kubelet needs the KEP-4639 image volume feature gate set
	// explicitly, and whether the kubeadm config renders as v1beta4. An empty
	// value leaves the gate unset and keeps v1beta3.
	KubernetesVersion string
	// KubeReservedMemory is the memory reserved for Kubernetes processes by the
	// kubelet configuration patch. It defaults to 2Gi when empty.
	KubeReservedMemory string
}

// NewJoinConfigFromString parses the join configuration from a kubeadm join string.
// The join string for a control-plane node should be in the format:
// `kubeadm join <api-endpoint> --control-plane --certificate-key <key> --token <token> --discovery-token-ca-cert-hash <hash>`.
// The join string for a worker node should be in the format:
// `kubeadm join <api-endpoint> --token <token> --discovery-token-ca-cert-hash <hash>`.
func NewJoinConfigFromString(nodeIP, nodeName, joinString string) (*JoinConfig, error) {
	joinString = strings.TrimSpace(joinString)

	if !strings.HasPrefix(joinString, "kubeadm join ") {
		return nil, fmt.Errorf("%w: join string must start with 'kubeadm join', got %q", ErrInvalidJoinString, joinString)
	}

	joinString = strings.TrimPrefix(joinString, "kubeadm join ")
	parts := strings.Fields(joinString)
	if len(parts) < 3 { // 3 is the minimum number of parts required
		return nil, fmt.Errorf("%w: expected at least 3 parts, got %d. Join string: %q", ErrInvalidJoinString, len(parts), joinString)
	}

	apiServerEndpoint := parts[0]
	parsedEndpoint, err := url.Parse("//" + apiServerEndpoint)
	if err != nil || parsedEndpoint.Hostname() == "" || strings.HasPrefix(apiServerEndpoint, "--") {
		apiServerEndpoint = ""
	}

	config := &JoinConfig{
		APIServerEndpoint: apiServerEndpoint,
		NodeIP:            nodeIP,
		NodeName:          nodeName,
	}

	for i := 1; i < len(parts); i++ {
		switch parts[i] {
		case "--control-plane":
			config.ControlPlane = true
		case "--certificate-key":
			if i+1 < len(parts) {
				config.CertificateKey = parts[i+1]
				i++
			}
		case "--token":
			if i+1 < len(parts) {
				config.Token = parts[i+1]
				i++
			}
		case "--discovery-token-ca-cert-hash":
			if i+1 < len(parts) {
				config.DiscoveryTokenCACertHash = parts[i+1]
				i++
			}
		}
	}
	// Set node taints. For now, we only taint worker nodes
	if config.ControlPlane {
		config.NodeTaints = nil
	} else {
		config.NodeTaints = []constants.NodeTaint{
			constants.NodeTaintProvisioning(),
		}
	}

	// Validate required fields
	if config.APIServerEndpoint == "" {
		return nil, ErrMissingAPIServerEndpoint
	}
	if config.Token == "" {
		return nil, ErrMissingToken
	}
	if config.DiscoveryTokenCACertHash == "" {
		return nil, ErrMissingHash
	}
	if config.ControlPlane && config.CertificateKey == "" {
		return nil, ErrMissingCertificateKey
	}
	if config.NodeName == "" {
		return nil, ErrMissingNodeName
	}

	return config, nil
}

// Render generates the bootstrap artifacts for kubeadm join.
func (config *JoinConfig) Render() (*BootstrapArtifacts, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid join configuration: %w", err)
	}

	configYAML, err := renderYAMLDocuments(
		joinKubeadmDocument(joinConfiguration(config), config.KubernetesVersion),
		kubeProxyConfiguration(false),
		joinKubeletConfiguration(config.KubernetesVersion),
	)
	if err != nil {
		return nil, fmt.Errorf("rendering kubeadm join yaml config: %w", err)
	}

	artifacts := &BootstrapArtifacts{
		ConfigYAML: configYAML,
		Files:      []File{kubeletConfigurationPatch(config.SystemReservedMemory, config.KubeReservedMemory)},
	}

	if config.ControlPlane {
		artifacts.Files = append(
			artifacts.Files,
			apiServerTerminationGracePatch(),
			apiServerAuditPolicyFile(),
		)
	}
	if config.ControlPlane && needsAuthenticationConfig(config.KubernetesVersion, config.AuthenticationConfig) {
		file, fileErr := GenerateAPIServerAuthenticationConfig(
			apiServerAuthenticationConfig(config.AuthenticationConfig),
			config.KubernetesVersion,
		)
		if fileErr != nil {
			return nil, fmt.Errorf("building AuthenticationConfig file: %w", fileErr)
		}
		artifacts.Files = append(artifacts.Files, file)
	}

	return artifacts, nil
}

func (config *JoinConfig) Validate() error {
	if config.ControlPlane {
		if config.NodeIP == "" {
			return errors.New("advertise address is required for control plane nodes")
		}
		if config.CertificateKey == "" {
			return errors.New("certificate key is required for control plane nodes")
		}
	}
	if config.APIServerEndpoint == "" {
		return errors.New("control plane endpoint is required")
	}
	if config.NodeName == "" {
		return errors.New("node name is required")
	}
	if config.DiscoveryTokenCACertHash == "" {
		return errors.New("discovery token CA cert hash is required")
	}
	if config.Token == "" {
		return errors.New("token is required")
	}
	return nil
}

func joinConfiguration(config *JoinConfig) kubeadmv1beta3.JoinConfiguration {
	cfg := kubeadmv1beta3.JoinConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: kubeadmv1beta3.APIVersion,
			Kind:       kubeadmv1beta3.JoinConfigurationKind,
		},
		Patches: &kubeadmv1beta3.Patches{
			Directory: defaultPatchesDirectory,
		},
		Discovery: kubeadmv1beta3.Discovery{
			BootstrapToken: &kubeadmv1beta3.BootstrapTokenDiscovery{
				APIServerEndpoint: config.APIServerEndpoint,
				CACertHashes:      []string{config.DiscoveryTokenCACertHash},
				Token:             config.Token,
			},
			TLSBootstrapToken: config.Token,
			Timeout:           &metav1.Duration{Duration: 5 * time.Minute},
		},
		NodeRegistration: kubeadmv1beta3.NodeRegistrationOptions{
			CRISocket: defaultCRISocket,
			Name:      config.NodeName,
			KubeletExtraArgs: map[string]string{
				"node-ip": config.NodeIP,
			},
			Taints: toKubeadmNodeTaints(config.NodeTaints),
		},
	}

	// Mirrors init: skip SystemVerification preflight on DinD nodes.
	if config.KubeadmContainerMode {
		cfg.NodeRegistration.IgnorePreflightErrors = []string{"SystemVerification"}
	}

	if config.ControlPlane {
		cfg.ControlPlane = &kubeadmv1beta3.JoinControlPlane{
			LocalAPIEndpoint: kubeadmv1beta3.APIEndpoint{
				AdvertiseAddress: config.NodeIP,
				BindPort:         int32(defaultBindPort),
			},
			CertificateKey: config.CertificateKey,
		}
	}

	return cfg
}
