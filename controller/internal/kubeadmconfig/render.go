// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package kubeadmconfig

import (
	"bytes"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/nke/controller/internal/constants"
	kubeadmv1beta3 "github.com/NVIDIA/nke/controller/internal/kubeadmconfig/thirdparty/kubeadmtypes/v1beta3"
	kubeletconfigv1beta1 "github.com/NVIDIA/nke/controller/internal/kubeadmconfig/thirdparty/kubeletconfig/v1beta1"
	kubeproxyconfigv1alpha1 "github.com/NVIDIA/nke/controller/internal/kubeadmconfig/thirdparty/kubeproxyconfig/v1alpha1"
)

const (
	defaultPatchesDirectory          = "/etc/kubernetes/patches"
	defaultClusterName               = "kubernetes"
	defaultDNSDomain                 = "cluster.local"
	defaultBindPort                  = 6443
	defaultCRISocket                 = "unix:///var/run/containerd/containerd.sock"
	defaultImagePullPolicy           = "IfNotPresent"
	defaultImageRepository           = "registry.k8s.io"
	defaultAuditPolicyFile           = "/etc/kubernetes/policies/audit-policy.yaml"
	defaultAuditLogPath              = "/var/log/kubernetes_audit/audit.log"
	defaultKubeletCertificateAuth    = "/etc/kubernetes/pki/ca.crt"
	defaultCertificatesDir           = "/etc/kubernetes/pki"
	defaultEtcdDataDir               = "/var/lib/etcd"
	defaultListenMetricsURLs         = "http://0.0.0.0:2381"
	defaultKubeProxyMetricsBind      = "0.0.0.0:10249"
	defaultBindAddress               = "0.0.0.0"
	defaultAuditLogMaxAge            = "30"
	defaultAuditLogMaxBackup         = "3"
	defaultAuditLogMaxSize           = "100"
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
)

const (
	const256Mi = "256Mi"
	memoryKey  = "memory"
	rootRoot   = "root:root"
)

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

func initKubeletConfiguration(containerMode bool) kubeletconfigv1beta1.KubeletConfiguration {
	cfg := kubeletconfigv1beta1.KubeletConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: kubeletconfigv1beta1.APIVersion,
			Kind:       kubeletconfigv1beta1.KubeletConfigurationKind,
		},
		ServerTLSBootstrap: true,
		KubeReserved: map[string]string{
			"cpu":               "1000m",
			memoryKey:           defaultKubeReservedMemory,
			"ephemeral-storage": "1Gi",
		},
		SystemReserved: map[string]string{
			memoryKey: defaultSystemReservedMemory,
		},
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

func joinKubeletConfiguration() kubeletconfigv1beta1.KubeletConfiguration {
	return kubeletconfigv1beta1.KubeletConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: kubeletconfigv1beta1.APIVersion,
			Kind:       kubeletconfigv1beta1.KubeletConfigurationKind,
		},
		ServerTLSBootstrap: true,
	}
}

// controlPlaneComponent returns ExtraArgs shared by all control-plane
// components (currently kube-controller-manager and kube-scheduler).
func controlPlaneComponent() kubeadmv1beta3.ControlPlaneComponent {
	return kubeadmv1beta3.ControlPlaneComponent{
		ExtraArgs: map[string]string{
			"profiling":    defaultProfilingDisabled,
			"bind-address": defaultBindAddress,
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
		Owner: rootRoot,
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
		Owner: rootRoot,
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
