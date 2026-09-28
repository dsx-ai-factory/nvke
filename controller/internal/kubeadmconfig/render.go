// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"bytes"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	kubeadmv1beta3 "github.com/dsx-ai-factory/nke/controller/internal/kubeadmconfig/thirdparty/kubeadmtypes/v1beta3"
	kubeletconfigv1beta1 "github.com/dsx-ai-factory/nke/controller/internal/kubeadmconfig/thirdparty/kubeletconfig/v1beta1"
	kubeproxyconfigv1alpha1 "github.com/dsx-ai-factory/nke/controller/internal/kubeadmconfig/thirdparty/kubeproxyconfig/v1alpha1"
)

const (
	defaultPatchesDirectory       = "/etc/kubernetes/patches"
	defaultClusterName            = "kubernetes"
	defaultDNSDomain              = "cluster.local"
	defaultBindPort               = 6443
	defaultCRISocket              = "unix:///var/run/containerd/containerd.sock"
	defaultImagePullPolicy        = "IfNotPresent"
	defaultImageRepository        = "registry.k8s.io"
	defaultAuditPolicyFile        = "/etc/kubernetes/policies/audit-policy.yaml"
	defaultAuditLogPath           = "/var/log/kubernetes_audit/audit.log"
	defaultKubeletCertificateAuth = "/etc/kubernetes/pki/ca.crt"
	defaultCertificatesDir        = "/etc/kubernetes/pki"
	defaultEtcdDataDir            = "/var/lib/etcd"
	defaultListenMetricsURLs      = "http://0.0.0.0:2381"
	defaultKubeProxyMetricsBind   = "0.0.0.0:10249"
	defaultBindAddress            = "0.0.0.0"
	defaultAuditLogMaxAge         = "30"
	// defaultControlPlaneBindAddress keeps kube-controller-manager and
	// kube-scheduler on loopback. Both serve only their own metrics and health
	// endpoints, and nothing outside the node consumes them, so binding all
	// interfaces exposed them for no gain (CIS 1.3.7 and 1.4.2). Deliberately
	// separate from defaultBindAddress, which the API server also uses and which
	// must keep listening on a routable address for nodes and clients to reach
	// it. A scraper that later needs these endpoints should reach them through a
	// localhost-scoped path rather than widening this back to 0.0.0.0.
	defaultControlPlaneBindAddress = "127.0.0.1"
	// defaultAuditLogMaxBackup is held at or above ten so a rotation storm cannot
	// age the audit trail out faster than it can be shipped. Three rotations of a
	// 100Mi log is well under a day of retained history on a busy API server,
	// which is what CIS 1.2.18 objects to.
	defaultAuditLogMaxBackup = "10"
	defaultAuditLogMaxSize   = "100"
	// defaultServiceAccountExtendTokenExpiration turns off the extended
	// (one-year) service account token lifetime, so projected tokens expire on
	// the bound schedule the cluster actually intends (CIS 1.2.30). The extension
	// exists to shield clients that cannot re-read a rotating token; NKE ships no
	// such client, and leaving it on silently grants every workload a year-long
	// credential.
	defaultServiceAccountExtendTokenExpiration = "false"
	// defaultTLSCipherSuites pins the TLS 1.2 handshake to AEAD suites with
	// forward secrecy, instead of accepting Go's broader default set (CIS 1.2.29).
	// TLS 1.3 suites are not configurable through this flag and are unaffected.
	defaultTLSCipherSuites = "TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384," +
		"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384," +
		"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256," +
		"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256," +
		"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256," +
		"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256"
	defaultProfilingDisabled         = "false"
	defaultAPIServerShutdownDelay    = "10s"
	defaultAPIServerShutdownRetry    = "true"
	defaultAPIServerShutdownWatch    = "20s"
	defaultAutoCompactionRetention   = "1"
	defaultEncryptionConfigPath      = "/etc/kubernetes/kube-api-encryption/encryption-config.yaml"
	defaultEncryptionConfigDirectory = "/etc/kubernetes/kube-api-encryption"
	defaultOIDCConfigDirectory       = "/etc/kubernetes/oidc"
	defaultAPIServerPatchFile        = defaultPatchesDirectory + "/kube-apiserver-base-graceful-shutdown+merge.yaml"
	defaultKubeletPatchFile          = defaultPatchesDirectory + "/kubeletconfiguration0+merge.yaml"
	defaultSystemReservedMemory      = "16Gi"
	defaultKubeReservedMemory        = "2Gi"
	defaultAPIServerTerminationGrace = 45
	// defaultEnabledAdmissionPlugins overrides kubeadm's own default
	// enable-admission-plugins value rather than adding to it — kubeadm merges
	// apiServer.extraArgs into its defaults by flag key, so the value supplied
	// here replaces kubeadm's default of "NodeRestriction" wholesale. It must
	// therefore include NodeRestriction explicitly to avoid dropping it. Each
	// of the other plugins is safe to enable unconditionally: DenyServiceExternalIPs
	// and OwnerReferencesPermissionEnforcement are self-contained; PodNodeSelector
	// and PodTolerationRestriction only activate for namespaces carrying
	// annotations NKE does not set today, so they are no-ops until paired with
	// that follow-up work.
	defaultEnabledAdmissionPlugins = "NodeRestriction,DenyServiceExternalIPs,OwnerReferencesPermissionEnforcement,PodNodeSelector,PodTolerationRestriction"
)

const (
	const256Mi = "256Mi"
	memoryKey  = "memory"

	// Kept modest: parallel pulls share the node's disk and network, and past
	// four the contention costs more than the queueing it removes.
	defaultMaxParallelImagePulls = 4

	// imageVolumeFeatureGate is the KEP-4639 OCI image volume gate. Manifests
	// that mount a plugin binary through an `image:` volume source need it on
	// both kube-apiserver (which otherwise strips the volume during admission,
	// silently) and kubelet (which otherwise cannot mount it).
	imageVolumeFeatureGate = "ImageVolume"

	// featureGatesFlag is the kube-apiserver flag carrying comma-separated
	// `Name=bool` pairs.
	featureGatesFlag = "feature-gates"

	// defaultPodPidsLimit caps PIDs per pod, which the kubelet otherwise leaves
	// unbounded so a single runaway process can exhaust the node's PID space
	// (CIS 4.2.13). Threads count against this cgroup limit, not just processes,
	// and GPU training pods run many dataloader workers and CUDA threads apiece
	// — so this sits well above a general-purpose default. The control asks only
	// that a limit exist; the value is chosen to bound a fork bomb without being
	// reachable by a legitimate workload.
	defaultPodPidsLimit = 16384
)

// kubeletTLSCipherSuites returns the TLS 1.2 cipher suites the kubelet's
// serving endpoint accepts, restricting it to AEAD suites with forward secrecy
// instead of Go's broader default set (CIS 4.2.12). TLS 1.3 suites are not
// configurable through this field and are unaffected.
//
// Returns a fresh slice per call: KubeletConfiguration holds it by reference
// and there is more than one call site, so a shared backing array could be
// mutated through one config and observed through the other.
func kubeletTLSCipherSuites() []string {
	return []string{
		"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
		"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
		"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
		"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
		"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256",
		"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256",
	}
}

// imageVolumeGAVersion is the release where KEP-4639 went GA and the gate
// became enabled by default. Kubernetes removes a gate roughly two releases
// after GA, and passing a removed gate stops the component from starting, so
// the flag is only set below this floor.
func imageVolumeGAVersion() *version.Version { return version.MustParseGeneric("1.36.0") }

// needsImageVolumeFeatureGate reports whether kubernetesVersion is old enough
// that KEP-4639 must be turned on explicitly.
//
// An unparseable version yields false on purpose. Omitting the gate leaves an
// image volume silently stripped, which is recoverable; passing an unknown or
// removed gate prevents kube-apiserver from starting at all, which is not.
func needsImageVolumeFeatureGate(kubernetesVersion string) bool {
	parsed, err := version.ParseGeneric(kubernetesVersion)
	if err != nil {
		return false
	}
	return parsed.LessThan(imageVolumeGAVersion())
}

// anonymousAuthConfigMinVersion is the first release that can consume the
// AuthenticationConfiguration this package renders. The gating fact is the API
// version, not the feature gate: renderAuthenticationConfig always stamps
// apiserver.config.k8s.io/v1, and AuthenticationConfiguration only joins that
// group version in 1.34. Through 1.33 the kind exists solely as v1alpha1 (1.29)
// and v1beta1 (1.30-1.33) -- 1.33's apiserver.config.k8s.io/v1 reference lists
// only AdmissionConfiguration, AuthorizationConfiguration and
// EncryptionConfiguration. Handing a 1.32 or 1.33 kube-apiserver a v1
// AuthenticationConfiguration means it cannot decode the file and does not
// start, which is the bringup break this whole change exists to avoid.
//
// AnonymousAuthConfigurableEndpoints is beta and on by default from 1.32, so it
// is not the binding constraint; the API version is. Lowering this floor
// therefore requires rendering v1beta1 for 1.32-1.33, not just a gate check.
func anonymousAuthConfigMinVersion() *version.Version { return version.MustParseGeneric("1.34.0") }

// supportsAnonymousAuthConfig reports whether kubernetesVersion understands the
// anonymous-restricted AuthenticationConfiguration.
//
// An unparseable version yields false on purpose, mirroring
// needsImageVolumeFeatureGate. Skipping the restriction leaves anonymous access
// where it already is, which is recoverable; handing kube-apiserver a document
// it cannot parse stops the control plane from starting at all.
func supportsAnonymousAuthConfig(kubernetesVersion string) bool {
	parsed, err := version.ParseGeneric(kubernetesVersion)
	if err != nil {
		return false
	}
	return !parsed.LessThan(anonymousAuthConfigMinVersion())
}

// needsAuthenticationConfig reports whether a control-plane node gets an
// AuthenticationConfiguration at all. OIDC still requires one on every release
// that supports it, so the anonymous restriction only widens the set of
// clusters that get the file and the flag; it never narrows it.
func needsAuthenticationConfig(kubernetesVersion string, oidc *OIDCAuthenticationConfig) bool {
	return oidc != nil || supportsAnonymousAuthConfig(kubernetesVersion)
}

// imageVolumeKubeletFeatureGates returns the kubelet featureGates map for
// kubernetesVersion, or nil when the gate is unnecessary. nil keeps the field
// omitted from the rendered YAML rather than emitting an empty map.
func imageVolumeKubeletFeatureGates(kubernetesVersion string) map[string]bool {
	if !needsImageVolumeFeatureGate(kubernetesVersion) {
		return nil
	}
	return map[string]bool{imageVolumeFeatureGate: true}
}

func renderYAMLDocuments(documents ...any) ([]byte, error) {
	var buf bytes.Buffer

	for i, document := range documents {
		if i > 0 {
			buf.WriteString("---\n")
		}

		rendered, err := yaml.Marshal(document)
		if err != nil {
			return nil, fmt.Errorf("marshaling YAML document %d: %w", i, err)
		}

		buf.Write(rendered)
	}

	return buf.Bytes(), nil
}

func kubeProxyConfiguration(containerMode bool) kubeproxyconfigv1alpha1.KubeProxyConfiguration {
	cfg := kubeproxyconfigv1alpha1.KubeProxyConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: kubeproxyconfigv1alpha1.APIVersion,
			Kind:       kubeproxyconfigv1alpha1.KubeProxyConfigurationKind,
		},
		MetricsBindAddress: defaultKubeProxyMetricsBind,
	}

	if containerMode {
		// Disable conntrack tuning: nf_conntrack_max is a non-namespaced sysctl
		// not writable from a container netns (docker-in-docker / Kind), so
		// kube-proxy's attempt to set it would fail. Setting maxPerCore and min
		// to 0 tells kube-proxy to skip writing the sysctl entirely.
		maxPerCore := int32(0)
		conntrackMin := int32(0)
		cfg.Conntrack.MaxPerCore = &maxPerCore
		cfg.Conntrack.Min = &conntrackMin
	}

	return cfg
}

func initKubeletConfiguration(containerMode bool, kubernetesVersion string) kubeletconfigv1beta1.KubeletConfiguration {
	cfg := kubeletconfigv1beta1.KubeletConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: kubeletconfigv1beta1.APIVersion,
			Kind:       kubeletconfigv1beta1.KubeletConfigurationKind,
		},
		ServerTLSBootstrap: true,
		// kubelet pulls one image at a time by default, so a node bringing up
		// several workloads queues them and the last one can miss its readiness
		// deadline.
		SerializeImagePulls:   ptr.To(false),
		MaxParallelImagePulls: ptr.To[int32](defaultMaxParallelImagePulls),
		TLSCipherSuites:       kubeletTLSCipherSuites(),
		PodPidsLimit:          ptr.To[int64](defaultPodPidsLimit),
		// RuntimeDefault seccomp for every workload that does not name a
		// profile of its own (CIS 4.2.14). Without this the kubelet runs
		// unconfined by default, so a pod gets the container runtime's full
		// syscall surface unless its author opted in. Pods that genuinely need
		// a wider surface can still say so explicitly in their securityContext;
		// this only changes what happens when nothing is specified.
		SeccompDefault: ptr.To(true),
		KubeReserved: map[string]string{
			"cpu":               "1000m",
			memoryKey:           defaultKubeReservedMemory,
			"ephemeral-storage": "1Gi",
		},
		SystemReserved: map[string]string{
			memoryKey: defaultSystemReservedMemory,
		},
		FeatureGates: imageVolumeKubeletFeatureGates(kubernetesVersion),
	}
	if containerMode {
		// kindest-as-Forge-node containers share the host kernel and
		// inherit its swap setting. kubelet refuses to start by default
		// when swap is on; in container-backed nodes we have no way to disable it.
		// Production VM/bare-metal nodes are untouched.
		falseVal := false
		cfg.FailSwapOn = &falseVal
		// Production reservations (16Gi system + 2Gi kube) exceed total
		// capacity of small dev VMs (e.g. Colima with 12Gi). kubelet refuses
		// to start when reservation > capacity. Shrink to nominal values
		// so the node container's allocatable memory remains positive.
		cfg.KubeReserved = map[string]string{
			"cpu":               "100m",
			memoryKey:           const256Mi,
			"ephemeral-storage": const256Mi,
		}
		cfg.SystemReserved = map[string]string{
			memoryKey: const256Mi,
		}
	}
	return cfg
}

func joinKubeletConfiguration(kubernetesVersion string) kubeletconfigv1beta1.KubeletConfiguration {
	return kubeletconfigv1beta1.KubeletConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: kubeletconfigv1beta1.APIVersion,
			Kind:       kubeletconfigv1beta1.KubeletConfigurationKind,
		},
		ServerTLSBootstrap:    true,
		SerializeImagePulls:   ptr.To(false),
		MaxParallelImagePulls: ptr.To[int32](defaultMaxParallelImagePulls),
		TLSCipherSuites:       kubeletTLSCipherSuites(),
		PodPidsLimit:          ptr.To[int64](defaultPodPidsLimit),
		SeccompDefault:        ptr.To(true),
		FeatureGates:          imageVolumeKubeletFeatureGates(kubernetesVersion),
	}
}

// controlPlaneComponent returns ExtraArgs shared by all control-plane
// components (currently kube-controller-manager and kube-scheduler).
func controlPlaneComponent() kubeadmv1beta3.ControlPlaneComponent {
	return kubeadmv1beta3.ControlPlaneComponent{
		ExtraArgs: map[string]string{
			"profiling":    defaultProfilingDisabled,
			"bind-address": defaultControlPlaneBindAddress,
		},
	}
}

// controllerManagerComponent extends controlPlaneComponent with flags that
// are specific to kube-controller-manager and would be rejected by the
// scheduler if included in the shared set.
func controllerManagerComponent(extraArgs map[string]string) kubeadmv1beta3.ControlPlaneComponent {
	cpc := controlPlaneComponent()
	for k, v := range extraArgs {
		cpc.ExtraArgs[k] = v
	}
	return cpc
}

func encryptionExtraArgs() (string, string) {
	return defaultEncryptionConfigPath, "true"
}

func apiServerTerminationGracePatch() File {
	// terminationGracePeriodSeconds is a kube-apiserver static Pod field, not
	// a kube-apiserver flag, so it has to be applied through a kubeadm patch.
	return File{
		Path:  defaultAPIServerPatchFile,
		Mode:  0o644,
		Owner: "root:root",
		Content: []byte(fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: kube-apiserver
spec:
  terminationGracePeriodSeconds: %d
`, defaultAPIServerTerminationGrace)),
	}
}

func kubeletConfigurationPatch(systemReservedMemory, kubeReservedMemory string) File {
	if systemReservedMemory == "" {
		systemReservedMemory = defaultSystemReservedMemory
	}
	if kubeReservedMemory == "" {
		kubeReservedMemory = defaultKubeReservedMemory
	}

	return File{
		Path:  defaultKubeletPatchFile,
		Mode:  0o644,
		Owner: "root:root",
		Content: []byte(fmt.Sprintf(`apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
kubeReserved:
  cpu: "1000m"
  memory: %q
  ephemeral-storage: "1Gi"
systemReserved:
  memory: %q
`, kubeReservedMemory, systemReservedMemory)),
	}
}

// encryptionVolume mounts the parent directory, not the file itself.
// A File-typed bind mount anchors to the file's inode, so tmp-file-
// then-rename writes during key rotation either hit a stale inode or
// EBUSY. Directory mounts follow the canonical dirent.
func encryptionVolume() kubeadmv1beta3.HostPathMount {
	return kubeadmv1beta3.HostPathMount{
		Name:      encryptionConfigVolumeName,
		HostPath:  defaultEncryptionConfigDirectory,
		MountPath: defaultEncryptionConfigDirectory,
		PathType:  corev1.HostPathDirectoryOrCreate,
		ReadOnly:  true,
	}
}

// authenticationConfigVolume uses the same directory-mount pattern as
// encryptionVolume so atomic tmp-file-then-rename writes don't strand
// the apiserver on a stale inode.
func authenticationConfigVolume() kubeadmv1beta3.HostPathMount {
	return kubeadmv1beta3.HostPathMount{
		Name:      AuthConfigVolumeName,
		HostPath:  defaultOIDCConfigDirectory,
		MountPath: defaultOIDCConfigDirectory,
		PathType:  corev1.HostPathDirectoryOrCreate,
		ReadOnly:  true,
	}
}

func toKubeadmNodeTaints(taints []constants.NodeTaint) []corev1.Taint {
	if len(taints) == 0 {
		return nil
	}

	out := make([]corev1.Taint, 0, len(taints))
	for _, taint := range taints {
		out = append(out, corev1.Taint{
			Key:    taint.Key,
			Value:  taint.Value,
			Effect: corev1.TaintEffect(taint.Effect),
		})
	}

	return out
}
