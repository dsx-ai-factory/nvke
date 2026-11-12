// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubebench

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const (
	// adminKubeconfig is the kubeconfig kubeadm writes on a control-plane
	// node. The policies target reads the cluster through it.
	adminKubeconfig = "/etc/kubernetes/admin.conf"
)

// hostMount is one host path kube-bench reads, mounted where kube-bench
// v0.16.0's job.yaml mounts it.
type hostMount struct {
	name, hostPath, mountPath string
}

// hostMounts are job.yaml's volumes, in its order.
func hostMounts() []hostMount {
	return []hostMount{
		{"var-lib-cni", "/var/lib/cni", "/var/lib/cni"},
		{"var-lib-etcd", "/var/lib/etcd", "/var/lib/etcd"},
		{"var-lib-kubelet", "/var/lib/kubelet", "/var/lib/kubelet"},
		{"var-lib-kube-scheduler", "/var/lib/kube-scheduler", "/var/lib/kube-scheduler"},
		{"var-lib-kube-controller-manager", "/var/lib/kube-controller-manager", "/var/lib/kube-controller-manager"},
		{"etc-systemd", "/etc/systemd", "/etc/systemd"},
		{"lib-systemd", "/lib/systemd", "/lib/systemd/"},
		{"srv-kubernetes", "/srv/kubernetes", "/srv/kubernetes/"},
		{"etc-kubernetes", "/etc/kubernetes", "/etc/kubernetes"},
		{"usr-bin", "/usr/bin", "/usr/local/mount-from-host/bin"},
		{"etc-cni-netd", "/etc/cni/net.d/", "/etc/cni/net.d/"},
		{"opt-cni-bin", "/opt/cni/bin/", "/opt/cni/bin/"},
	}
}

// Targets are the kube-bench targets a node pod runs: every control-plane
// target on a control-plane node, and the node target on a worker. The
// policies target runs in its own pod; see NewPoliciesPod.
func Targets(role Role) []string {
	if role == RoleControlPlane {
		return []string{"master", "controlplane", "etcd", "node"}
	}
	return []string{"node"}
}

// kubeBenchArgs are kube-bench's arguments for some targets: the pinned
// benchmark from the mounted files, as JSON.
func kubeBenchArgs(targets []string) []string {
	return []string{
		"--config-dir", ConfigDir,
		"--benchmark", Benchmark,
		"run", "--targets", strings.Join(targets, ","),
		"--json",
	}
}

// NewPod is the scan pod for one node. It shares the host's PID namespace,
// tolerates every taint, mounts the host paths kube-bench reads and the
// benchmark files, and prints the report as JSON.
func NewPod(namespace, node string, benchmark corev1.Volume, role Role) *corev1.Pod {
	mounts := hostMounts()
	volumes := make([]corev1.Volume, 0, len(mounts)+1)
	volumeMounts := make([]corev1.VolumeMount, 0, len(mounts)+1)
	for _, m := range mounts {
		volumes = append(volumes, corev1.Volume{
			Name:         m.name,
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: m.hostPath}},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: m.name, MountPath: m.mountPath, ReadOnly: true})
	}
	volumes = append(volumes, benchmark)
	volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: benchmark.Name, MountPath: ConfigDir, ReadOnly: true})

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "kube-bench-",
			Namespace:    namespace,
			Labels:       map[string]string{PodLabel: PodLabelValue},
		},
		Spec: corev1.PodSpec{
			NodeName:      node,
			HostPID:       true,
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations:   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:         PodContainer,
				Image:        Image,
				Command:      []string{"kube-bench"},
				Args:         kubeBenchArgs(Targets(role)),
				VolumeMounts: volumeMounts,
			}},
			Volumes: volumes,
		},
	}
}

// PoliciesServiceAccount is the policies pod's service account. It mounts no
// token: the pod reads the cluster through the node's admin kubeconfig, and
// section 5 counts a pod as mounting no token only when its service account
// does not either.
func PoliciesServiceAccount(namespace string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "kube-bench-policies-",
			Namespace:    namespace,
			Labels:       map[string]string{PodLabel: PodLabelValue},
		},
		AutomountServiceAccountToken: ptr.To(false),
	}
}

// NewPoliciesPod is the pod that runs the policies target on a control-plane
// node, through the node's admin kubeconfig, as serviceAccount (see
// PoliciesServiceAccount). Section 5 lists the cluster's pods, so this pod
// carries none of what it checks for: it does not share the host's PID
// namespace, mounts no service account token, and mounts nothing from the
// host but the kubeconfig. The scan runs it alone, before the node pods.
func NewPoliciesPod(namespace, node, serviceAccount string, benchmark corev1.Volume) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "kube-bench-policies-",
			Namespace:    namespace,
			Labels:       map[string]string{PodLabel: PodLabelValue},
		},
		Spec: corev1.PodSpec{
			NodeName:                     node,
			ServiceAccountName:           serviceAccount,
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr.To(false),
			Tolerations:                  []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:    PodContainer,
				Image:   Image,
				Command: []string{"kube-bench"},
				Args:    kubeBenchArgs([]string{"policies"}),
				Env:     []corev1.EnvVar{{Name: "KUBECONFIG", Value: adminKubeconfig}},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "admin-kubeconfig", MountPath: adminKubeconfig, ReadOnly: true},
					{Name: benchmark.Name, MountPath: ConfigDir, ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "admin-kubeconfig", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: adminKubeconfig}}},
				benchmark,
			},
		},
	}
}
