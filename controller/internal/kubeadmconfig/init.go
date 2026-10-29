// SPDX-FileCopyrightText: Copyright (c) 2024 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	bootstraptokenv1 "github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig/thirdparty/bootstraptoken/v1"
	kubeadmv1beta3 "github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig/thirdparty/kubeadmtypes/v1beta3"
)

const (
	EtcdQuotaBackendBytes            = "17179869184" // 16 GB
	DefaultSecondaryNodeEvictionRate = "0"
	DefaultLargeClusterSizeThreshold = "50"
)

// InitConfig represents the configuration for kubeadm init.
type InitConfig struct {
	NodeIP                string
	APIServerEndpoint     string
	NodeName              string
	KubernetesVersion     string
	PodSubnet             string
	ServiceSubnet         string
	EtcdQuotaBackendBytes string
	// Token is the kubeadm token used for joining nodes to the cluster.
	// If empty, a new token will be generated.
	Token string
	// ImageRepository is the OCI registry from which kubeadm pulls control-plane
	// component images. Defaults to registry.k8s.io when empty, allowing air-gap
	// deployments to redirect to a local mirror.
	ImageRepository string
	// SkipKubeProxy specifies whether or not to install kube-proxy addon
	SkipKubeProxy bool
	// EnableEncryption specifies whether to enable etcd encryption for kube-api
	EnableEncryption bool
	// KubeadmContainerMode enables workarounds for kubeadm running inside a
	// container-backed node, such as CAPD DockerMachine nodes.
	KubeadmContainerMode bool
	// SystemReservedMemory is the memory reserved for system processes by the
	// kubelet configuration patch. It defaults to 16Gi when empty.
	SystemReservedMemory string
	// KubeReservedMemory is the memory reserved for Kubernetes processes by the
	// kubelet configuration patch. It defaults to 2Gi when empty.
	KubeReservedMemory string
	// AuthenticationConfig configures the authentication-config flag in the
	// api server and generates the proper AuthenticationConfig file for OIDC.
	AuthenticationConfig *OIDCAuthenticationConfig
	// SecondaryNodeEvictionRate sets the kube-controller-manager
	// --secondary-node-eviction-rate flag, which controls the pod eviction
	// rate when a zone is unhealthy (≥55% of nodes NotReady). Defaults to "0"
	// to stop evictions during zone-wide events on single-zone NVKE clusters.
	SecondaryNodeEvictionRate string
	// LargeClusterSizeThreshold sets the kube-controller-manager
	// --large-cluster-size-threshold flag. Clusters at or below this size
	// get a hardcoded eviction rate of 0 during zone-wide events, bypassing
	// --secondary-node-eviction-rate entirely. Defaults to "50" (upstream
	// default).
	LargeClusterSizeThreshold string
}

// Render generates the bootstrap artifacts for kubeadm init.
func (config *InitConfig) Render() (*BootstrapArtifacts, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid init configuration: %w", err)
	}

	// Work on a copy so callers are not mutated.
	cfg := *config
	if cfg.Token == "" {
		token, err := GenerateToken()
		if err != nil {
			return nil, fmt.Errorf("generating token: %w", err)
		}
		cfg.Token = token
	}
	if cfg.ImageRepository == "" {
		cfg.ImageRepository = defaultImageRepository
	}
	if cfg.EtcdQuotaBackendBytes == "" {
		cfg.EtcdQuotaBackendBytes = EtcdQuotaBackendBytes
	}
	if cfg.SecondaryNodeEvictionRate == "" {
		cfg.SecondaryNodeEvictionRate = DefaultSecondaryNodeEvictionRate
	}
	if cfg.LargeClusterSizeThreshold == "" {
		cfg.LargeClusterSizeThreshold = DefaultLargeClusterSizeThreshold
	}

	initConfig, err := initConfiguration(&cfg)
	if err != nil {
		return nil, fmt.Errorf("building init configuration: %w", err)
	}

	initDocument, clusterDocument := initKubeadmDocuments(initConfig, clusterConfiguration(&cfg), cfg.KubernetesVersion)
	configYAML, err := renderYAMLDocuments(
		initDocument,
		clusterDocument,
		kubeProxyConfiguration(cfg.KubeadmContainerMode),
		initKubeletConfiguration(cfg.KubeadmContainerMode, cfg.KubernetesVersion),
	)
	if err != nil {
		return nil, fmt.Errorf("rendering kubeadm init yaml config: %w", err)
	}

	artifacts := &BootstrapArtifacts{
		ConfigYAML: configYAML,
		Files: []File{
			kubeletConfigurationPatch(cfg.SystemReservedMemory, cfg.KubeReservedMemory),
			apiServerTerminationGracePatch(),
			apiServerAuditPolicyFile(),
		},
	}

	if needsAuthenticationConfig(cfg.KubernetesVersion, cfg.AuthenticationConfig) {
		file, err := GenerateAPIServerAuthenticationConfig(
			apiServerAuthenticationConfig(cfg.AuthenticationConfig),
			cfg.KubernetesVersion,
		)
		if err != nil {
			return nil, fmt.Errorf("building AuthenticationConfig file: %w", err)
		}
		artifacts.Files = append(artifacts.Files, file)
	}

	return artifacts, nil
}

func (config *InitConfig) Validate() error {
	if config.NodeIP == "" {
		return errors.New("advertise address is required")
	}
	if config.APIServerEndpoint == "" {
		return errors.New("control plane endpoint is required")
	}
	if config.NodeName == "" {
		return errors.New("node name is required")
	}
	if config.KubernetesVersion == "" {
		return errors.New("kubernetes version is required")
	}
	if config.PodSubnet == "" {
		return errors.New("pod subnet is required")
	}
	return nil
}

func initConfiguration(config *InitConfig) (kubeadmv1beta3.InitConfiguration, error) {
	cfg := kubeadmv1beta3.InitConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: kubeadmv1beta3.APIVersion,
			Kind:       kubeadmv1beta3.InitConfigurationKind,
		},
		Patches: &kubeadmv1beta3.Patches{
			Directory: defaultPatchesDirectory,
		},
		LocalAPIEndpoint: kubeadmv1beta3.APIEndpoint{
			AdvertiseAddress: config.NodeIP,
			BindPort:         int32(defaultBindPort),
		},
		NodeRegistration: kubeadmv1beta3.NodeRegistrationOptions{
			CRISocket:       defaultCRISocket,
			ImagePullPolicy: corev1.PullPolicy(defaultImagePullPolicy),
			Name:            config.NodeName,
			KubeletExtraArgs: map[string]string{
				"node-ip": config.NodeIP,
			},
		},
	}

	// Skip kubeadm's SystemVerification preflight on DinD nodes; the
	// kindest container has no /lib/modules/$(uname -r) to probe against.
	if config.KubeadmContainerMode {
		cfg.NodeRegistration.IgnorePreflightErrors = []string{"SystemVerification"}
	}

	if config.Token != "" {
		bootstrapToken, err := bootstraptokenv1.NewBootstrapTokenString(config.Token)
		if err != nil {
			return kubeadmv1beta3.InitConfiguration{}, fmt.Errorf("invalid bootstrap token: %w", err)
		}

		cfg.BootstrapTokens = []bootstraptokenv1.BootstrapToken{
			{
				Token:  bootstrapToken,
				Groups: []string{"system:bootstrappers:kubeadm:default-node-token"},
				TTL:    &metav1.Duration{Duration: 24 * time.Hour},
				Usages: []string{"signing", "authentication"},
			},
		}
	}

	if config.SkipKubeProxy {
		cfg.SkipPhases = []string{"addon/kube-proxy"}
	}

	return cfg, nil
}

func clusterConfiguration(config *InitConfig) kubeadmv1beta3.ClusterConfiguration {
	apiServer := kubeadmv1beta3.APIServer{
		ControlPlaneComponent: kubeadmv1beta3.ControlPlaneComponent{
			ExtraArgs: map[string]string{
				"enable-admission-plugins":                defaultEnabledAdmissionPlugins,
				"audit-policy-file":                       defaultAuditPolicyFile,
				"audit-log-path":                          defaultAuditLogPath,
				"audit-log-maxage":                        defaultAuditLogMaxAge,
				"audit-log-maxbackup":                     defaultAuditLogMaxBackup,
				"audit-log-maxsize":                       defaultAuditLogMaxSize,
				"kubelet-certificate-authority":           defaultKubeletCertificateAuth,
				"profiling":                               defaultProfilingDisabled,
				"bind-address":                            defaultBindAddress,
				"service-account-extend-token-expiration": defaultServiceAccountExtendTokenExpiration,
				"tls-cipher-suites":                       defaultTLSCipherSuites,
				"shutdown-delay-duration":                 defaultAPIServerShutdownDelay,
				"shutdown-send-retry-after":               defaultAPIServerShutdownRetry,
				"shutdown-watch-termination-grace-period": defaultAPIServerShutdownWatch,
			},
			ExtraVolumes: []kubeadmv1beta3.HostPathMount{
				{
					Name:      "audit-logs",
					HostPath:  "/var/log/kubernetes_audit",
					MountPath: "/var/log/kubernetes_audit",
					PathType:  corev1.HostPathDirectoryOrCreate,
					ReadOnly:  false,
				},
				{
					Name:      "audit-policies",
					HostPath:  "/etc/kubernetes/policies",
					MountPath: "/etc/kubernetes/policies",
					PathType:  corev1.HostPathDirectoryOrCreate,
					ReadOnly:  true,
				},
			},
		},
		TimeoutForControlPlane: &metav1.Duration{Duration: 4 * time.Minute},
	}

	// KEP-4639 image volumes are stripped during admission when the gate is off,
	// so a manifest mounting a plugin through an `image:` volume fails with a
	// missing-file error far from the cause. Only set below the GA floor: the
	// flag is removed a couple of releases after GA and kube-apiserver refuses
	// to start when passed a gate it no longer knows.
	if needsImageVolumeFeatureGate(config.KubernetesVersion) {
		apiServer.ExtraArgs[featureGatesFlag] = imageVolumeFeatureGate + "=true"
	}

	if config.EnableEncryption {
		encryptionPath, reload := encryptionExtraArgs()
		apiServer.ExtraArgs["encryption-provider-config"] = encryptionPath
		apiServer.ExtraArgs["encryption-provider-config-automatic-reload"] = reload
		apiServer.ExtraVolumes = append(apiServer.ExtraVolumes, encryptionVolume())
	}
	// The AuthenticationConfiguration carries the anonymous-access restriction
	// (CIS 1.2.1) whether or not OIDC is configured, so every cluster on a
	// release that understands it gets the flag and the volume -- not just the
	// OIDC ones. Below that floor only OIDC pulls the flag in, as before.
	if needsAuthenticationConfig(config.KubernetesVersion, config.AuthenticationConfig) {
		apiServer.ExtraArgs[AuthConfigFlagName] = defaultOIDCAuthConfigPath
		apiServer.ExtraVolumes = append(apiServer.ExtraVolumes, authenticationConfigVolume())
	}

	return kubeadmv1beta3.ClusterConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: kubeadmv1beta3.APIVersion,
			Kind:       kubeadmv1beta3.ClusterConfigurationKind,
		},
		APIServer:       apiServer,
		CertificatesDir: defaultCertificatesDir,
		ControllerManager: controllerManagerComponent(map[string]string{
			"secondary-node-eviction-rate": config.SecondaryNodeEvictionRate,
			"large-cluster-size-threshold": config.LargeClusterSizeThreshold,
		}),
		ControlPlaneEndpoint: config.APIServerEndpoint,
		ClusterName:          defaultClusterName,
		Etcd: kubeadmv1beta3.Etcd{
			Local: &kubeadmv1beta3.LocalEtcd{
				DataDir: defaultEtcdDataDir,
				ExtraArgs: map[string]string{
					"quota-backend-bytes":       config.EtcdQuotaBackendBytes,
					"auto-compaction-retention": defaultAutoCompactionRetention,
					"listen-metrics-urls":       defaultListenMetricsURLs,
				},
			},
		},
		ImageRepository:   config.ImageRepository,
		KubernetesVersion: config.KubernetesVersion,
		Networking: kubeadmv1beta3.Networking{
			DNSDomain:     defaultDNSDomain,
			ServiceSubnet: config.ServiceSubnet,
			PodSubnet:     kubeadmPodSubnet(config.PodSubnet),
		},
		Scheduler: controlPlaneComponent(),
	}
}

func kubeadmPodSubnet(podSubnet string) string {
	parts := strings.Split(podSubnet, ",")
	selected := make([]string, 0, len(parts))
	seenIPv4 := false
	seenIPv6 := false

	for _, part := range parts {
		part = strings.TrimSpace(part)
		prefix, err := netip.ParsePrefix(part)
		if err != nil {
			selected = append(selected, part)
			continue
		}

		switch {
		case prefix.Addr().Is4():
			if seenIPv4 {
				continue
			}
			seenIPv4 = true
		case prefix.Addr().Is6():
			if seenIPv6 {
				continue
			}
			seenIPv6 = true
		}

		selected = append(selected, prefix.Masked().String())
	}

	return strings.Join(selected, ",")
}
