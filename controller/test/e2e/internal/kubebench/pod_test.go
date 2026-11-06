// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubebench

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestTargets(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{"master", "controlplane", "etcd", "node"}, Targets(RoleControlPlane))
	assert.Equal(t, []string{"node"}, Targets(RoleWorker))
}

func TestNewPod(t *testing.T) {
	t.Parallel()

	benchmark := BenchmarkVolume("kube-bench-cfg-x7k2p", syntheticBenchmark())

	// kube-bench v0.16.0's job.yaml: host path to mount path, all read-only,
	// no hostPath type.
	wantHostMounts := map[string]string{
		"/var/lib/cni":                     "/var/lib/cni",
		"/var/lib/etcd":                    "/var/lib/etcd",
		"/var/lib/kubelet":                 "/var/lib/kubelet",
		"/var/lib/kube-scheduler":          "/var/lib/kube-scheduler",
		"/var/lib/kube-controller-manager": "/var/lib/kube-controller-manager",
		"/etc/systemd":                     "/etc/systemd",
		"/lib/systemd":                     "/lib/systemd/",
		"/srv/kubernetes":                  "/srv/kubernetes/",
		"/etc/kubernetes":                  "/etc/kubernetes",
		"/usr/bin":                         "/usr/local/mount-from-host/bin",
		"/etc/cni/net.d/":                  "/etc/cni/net.d/",
		"/opt/cni/bin/":                    "/opt/cni/bin/",
	}

	tests := []struct {
		name        string
		role        Role
		wantTargets string
	}{
		{name: "control plane", role: RoleControlPlane, wantTargets: "master,controlplane,etcd,node"},
		{name: "worker", role: RoleWorker, wantTargets: "node"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pod := NewPod("default", "node-a", benchmark, tc.role)

			assert.Equal(t, "default", pod.Namespace)
			assert.Equal(t, "kube-bench-", pod.GenerateName)
			assert.Equal(t, PodLabelValue, pod.Labels[PodLabel])
			assert.Equal(t, "node-a", pod.Spec.NodeName)
			assert.True(t, pod.Spec.HostPID)
			assert.Equal(t, corev1.RestartPolicyNever, pod.Spec.RestartPolicy)
			assert.Equal(t, []corev1.Toleration{{Operator: corev1.TolerationOpExists}}, pod.Spec.Tolerations)

			require.Len(t, pod.Spec.Containers, 1)
			container := pod.Spec.Containers[0]
			assert.Equal(t, PodContainer, container.Name)
			assert.Equal(t, Image, container.Image)
			assert.Equal(t, []string{"kube-bench"}, container.Command)
			assert.Equal(t, []string{
				"--config-dir", ConfigDir, "--benchmark", "cis-2.0", "run", "--targets", tc.wantTargets, "--json",
			}, container.Args)
			assert.Empty(t, container.Env)

			mounts := map[string]corev1.VolumeMount{}
			for _, mount := range container.VolumeMounts {
				assert.True(t, mount.ReadOnly, mount.Name)
				mounts[mount.Name] = mount
			}
			gotHostMounts := map[string]string{}
			var gotBenchmark bool
			for _, volume := range pod.Spec.Volumes {
				mount, ok := mounts[volume.Name]
				require.True(t, ok, "volume %s is not mounted", volume.Name)
				switch {
				case volume.HostPath != nil:
					assert.Nil(t, volume.HostPath.Type, volume.Name)
					gotHostMounts[volume.HostPath.Path] = mount.MountPath
				case volume.ConfigMap != nil:
					assert.Equal(t, benchmark, volume)
					assert.Equal(t, ConfigDir, mount.MountPath)
					gotBenchmark = true
				default:
					t.Errorf("volume %s is neither a host path nor the benchmark", volume.Name)
				}
			}
			assert.Equal(t, wantHostMounts, gotHostMounts)
			assert.True(t, gotBenchmark, "the benchmark files are not mounted")
			assert.Len(t, container.VolumeMounts, len(pod.Spec.Volumes))
		})
	}
}

// The policies pod reads the cluster, not the host, so it carries none of
// what section 5 checks pods for: it does not share the host's PID namespace
// and mounts no service account token.
func TestNewPoliciesPod(t *testing.T) {
	t.Parallel()

	benchmark := BenchmarkVolume("kube-bench-cfg-x7k2p", syntheticBenchmark())
	pod := NewPoliciesPod("default", "cp-a", "kube-bench-policies-sa7xk", benchmark)

	assert.Equal(t, "default", pod.Namespace)
	assert.Equal(t, "kube-bench-policies-", pod.GenerateName)
	assert.Equal(t, PodLabelValue, pod.Labels[PodLabel])
	assert.Equal(t, "cp-a", pod.Spec.NodeName)
	assert.Equal(t, "kube-bench-policies-sa7xk", pod.Spec.ServiceAccountName)
	assert.False(t, pod.Spec.HostPID)
	require.NotNil(t, pod.Spec.AutomountServiceAccountToken)
	assert.False(t, *pod.Spec.AutomountServiceAccountToken)
	assert.Equal(t, corev1.RestartPolicyNever, pod.Spec.RestartPolicy)
	assert.Equal(t, []corev1.Toleration{{Operator: corev1.TolerationOpExists}}, pod.Spec.Tolerations)

	require.Len(t, pod.Spec.Containers, 1)
	container := pod.Spec.Containers[0]
	assert.Equal(t, PodContainer, container.Name)
	assert.Equal(t, Image, container.Image)
	assert.Equal(t, []string{"kube-bench"}, container.Command)
	assert.Equal(t, []string{
		"--config-dir", ConfigDir, "--benchmark", "cis-2.0", "run", "--targets", "policies", "--json",
	}, container.Args)
	assert.Equal(t, []corev1.EnvVar{{Name: "KUBECONFIG", Value: "/etc/kubernetes/admin.conf"}}, container.Env)

	mounts := map[string]corev1.VolumeMount{}
	for _, mount := range container.VolumeMounts {
		assert.True(t, mount.ReadOnly, mount.Name)
		mounts[mount.Name] = mount
	}
	require.Len(t, pod.Spec.Volumes, 2, "the admin kubeconfig and the benchmark files, nothing else from the host")
	for _, volume := range pod.Spec.Volumes {
		mount, ok := mounts[volume.Name]
		require.True(t, ok, "volume %s is not mounted", volume.Name)
		switch {
		case volume.HostPath != nil:
			assert.Equal(t, "/etc/kubernetes/admin.conf", volume.HostPath.Path)
			assert.Equal(t, "/etc/kubernetes/admin.conf", mount.MountPath)
		case volume.ConfigMap != nil:
			assert.Equal(t, benchmark, volume)
			assert.Equal(t, ConfigDir, mount.MountPath)
		default:
			t.Errorf("unexpected volume %s", volume.Name)
		}
	}
}

func TestPoliciesServiceAccount(t *testing.T) {
	t.Parallel()

	sa := PoliciesServiceAccount("default")
	assert.Equal(t, "default", sa.Namespace)
	assert.Equal(t, "kube-bench-policies-", sa.GenerateName)
	assert.Equal(t, PodLabelValue, sa.Labels[PodLabel])
	require.NotNil(t, sa.AutomountServiceAccountToken)
	assert.False(t, *sa.AutomountServiceAccountToken)
}
