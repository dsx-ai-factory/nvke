// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/kube"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/nke"
)

const (
	nodeLocalDNSName       = "node-local-dns"
	kubeSystemNamespace    = "kube-system"
	auditPolicyPath        = "/etc/kubernetes/policies/audit-policy.yaml"
	auditPolicyVolumeName  = "audit-policies"
	kubeReservedMemory     = "2Gi"
	dnsProbeContainerName  = "dns-probe"
	dnsProbeSuccessMessage = "node-local DNS resolved kubernetes.default"
	etcdQuotaBackendBytes  = "17179869184"
)

// validatePostReadyClusterState runs after the shared readiness spec, so
// every assertion observes the tenant-side effect only after the cluster and
// its complete desired topology have become Ready.
func validatePostReadyClusterState(test *tenantTest) {
	It("validates intended cluster state after all nodes are Ready", func(ctx context.Context) {
		tenantClient := expectSucceed(test.tenantClients.GetClient(ctx, test.cluster.Name))
		adminClient := expectSucceed(test.tenantClients.GetAdminClient(ctx, test.cluster.Name))
		expectedNodes := clusterNodeCount(test.spec)
		expectedControlPlanes := controlPlaneNodeCount(test.spec)

		By("Verifying node-local DNS is fully available on every Ready node")
		Eventually(func(ctx context.Context) error {
			return assertNodeLocalDNSAvailable(ctx, tenantClient, expectedNodes)
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())

		By("Resolving the Kubernetes Service through node-local DNS")
		Expect(runNodeLocalDNSProbe(ctx, adminClient)).To(Succeed())

		By("Verifying kubeadm and every Ready etcd mirror pod use the same quota")
		Expect(assertEtcdQuota(ctx, tenantClient, expectedControlPlanes)).To(Succeed())

		By("Verifying every Ready kube-apiserver pod uses the staged audit policy")
		Expect(assertAPIServerAuditPolicy(ctx, tenantClient, expectedControlPlanes)).To(Succeed())

		By("Verifying kubeadm patches reserve the requested memory on every Ready node")
		Expect(assertSystemReservedMemory(ctx, tenantClient, test.spec)).To(Succeed())
	}, Label("lifecycle", "post-ready"), NodeTimeout(15*time.Minute))
}

func assertNodeLocalDNSAvailable(ctx context.Context, tenantClient client.Client, expectedNodes int) error {
	var daemonSet appsv1.DaemonSet
	if err := tenantClient.Get(ctx, client.ObjectKey{Namespace: kubeSystemNamespace, Name: nodeLocalDNSName}, &daemonSet); err != nil {
		return fmt.Errorf("getting node-local DNS DaemonSet: %w", err)
	}
	status := daemonSet.Status
	if int(status.DesiredNumberScheduled) != expectedNodes {
		return fmt.Errorf("node-local DNS desired pods = %d, want %d Ready nodes", status.DesiredNumberScheduled, expectedNodes)
	}
	if status.NumberReady != status.DesiredNumberScheduled || status.NumberAvailable != status.DesiredNumberScheduled {
		return fmt.Errorf("node-local DNS pods are not fully available: desired=%d ready=%d available=%d unavailable=%d",
			status.DesiredNumberScheduled, status.NumberReady, status.NumberAvailable, status.NumberUnavailable)
	}
	return nil
}

func runNodeLocalDNSProbe(ctx context.Context, tenantClient client.Client) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-node-local-dns-",
			Namespace:    "default",
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    dnsProbeContainerName,
				Image:   kube.KubectlImage,
				Command: []string{"sh", "-ec"},
				Args: []string{`nslookup kubernetes.default.svc.cluster.local >/dev/null
printf '%s' "` + dnsProbeSuccessMessage + `" > /dev/termination-log`},
			}},
		},
	}
	_, message, err := kube.RunPod(ctx, tenantClient, pod, dnsProbeContainerName, 2*time.Minute)
	if err != nil {
		return err
	}
	if message != dnsProbeSuccessMessage {
		return fmt.Errorf("DNS probe message = %q, want %q", message, dnsProbeSuccessMessage)
	}
	return nil
}

func assertEtcdQuota(ctx context.Context, tenantClient client.Client, expectedControlPlanes int) error {
	expectedQuota, err := etcdQuotaInKubeadmConfig(ctx, tenantClient)
	if err != nil {
		return fmt.Errorf("reading etcd quota from kubeadm-config: %w", err)
	}
	if expectedQuota != etcdQuotaBackendBytes {
		return fmt.Errorf("kubeadm-config etcd quota = %q, want %q", expectedQuota, etcdQuotaBackendBytes)
	}

	var pods corev1.PodList
	if err := tenantClient.List(ctx, &pods,
		client.InNamespace(kubeSystemNamespace),
		client.MatchingLabels{"component": "etcd"},
	); err != nil {
		return fmt.Errorf("listing etcd mirror pods: %w", err)
	}
	if len(pods.Items) != expectedControlPlanes {
		return fmt.Errorf("found %d etcd mirror pods, want %d", len(pods.Items), expectedControlPlanes)
	}
	for _, pod := range pods.Items {
		if !podReady(&pod) {
			return fmt.Errorf("etcd mirror pod %s is not Ready", pod.Name)
		}
		quota, err := containerFlagValue(pod.Spec.Containers, "etcd", "quota-backend-bytes")
		if err != nil {
			return fmt.Errorf("reading etcd mirror pod %s quota: %w", pod.Name, err)
		}
		if quota != expectedQuota {
			return fmt.Errorf("etcd mirror pod %s quota = %q, want kubeadm-config value %q", pod.Name, quota, expectedQuota)
		}
	}
	return nil
}

func etcdQuotaInKubeadmConfig(ctx context.Context, tenantClient client.Client) (string, error) {
	var configMap corev1.ConfigMap
	if err := tenantClient.Get(ctx, client.ObjectKey{Namespace: kubeSystemNamespace, Name: kubeadmConfigMapName}, &configMap); err != nil {
		return "", fmt.Errorf("getting kubeadm ConfigMap: %w", err)
	}
	clusterConfiguration := configMap.Data[kubeadmConfigDataKey]
	if strings.TrimSpace(clusterConfiguration) == "" {
		return "", fmt.Errorf("kubeadm ConfigMap is missing non-empty %q data", kubeadmConfigDataKey)
	}

	configJSON, err := yaml.YAMLToJSON([]byte(clusterConfiguration))
	if err != nil {
		return "", fmt.Errorf("converting kubeadm ClusterConfiguration to JSON: %w", err)
	}
	var config struct {
		Etcd struct {
			Local struct {
				ExtraArgs any `json:"extraArgs"`
			} `json:"local"`
		} `json:"etcd"`
	}
	if err := json.Unmarshal(configJSON, &config); err != nil {
		return "", fmt.Errorf("decoding kubeadm ClusterConfiguration: %w", err)
	}

	const quotaName = "quota-backend-bytes"
	switch extraArgs := config.Etcd.Local.ExtraArgs.(type) {
	case map[string]any:
		if quota, ok := kubeadmArgValue(extraArgs[quotaName]); ok && quota != "" {
			return quota, nil
		}
	case []any:
		for _, item := range extraArgs {
			entry, ok := item.(map[string]any)
			if !ok || entry["name"] != quotaName {
				continue
			}
			if quota, ok := kubeadmArgValue(entry["value"]); ok && quota != "" {
				return quota, nil
			}
		}
	}
	return "", fmt.Errorf("kubeadm ClusterConfiguration is missing etcd.local.extraArgs[%q]", quotaName)
}

func assertAPIServerAuditPolicy(ctx context.Context, tenantClient client.Client, expectedControlPlanes int) error {
	var pods corev1.PodList
	if err := tenantClient.List(ctx, &pods,
		client.InNamespace(kubeSystemNamespace),
		client.MatchingLabels{"component": "kube-apiserver"},
	); err != nil {
		return fmt.Errorf("listing kube-apiserver mirror pods: %w", err)
	}
	if len(pods.Items) != expectedControlPlanes {
		return fmt.Errorf("found %d kube-apiserver mirror pods, want %d", len(pods.Items), expectedControlPlanes)
	}

	for _, pod := range pods.Items {
		if !podReady(&pod) {
			return fmt.Errorf("kube-apiserver mirror pod %s is not Ready", pod.Name)
		}
		path, err := containerFlagValue(pod.Spec.Containers, "kube-apiserver", "audit-policy-file")
		if err != nil {
			return fmt.Errorf("reading kube-apiserver mirror pod %s audit flag: %w", pod.Name, err)
		}
		if path != auditPolicyPath {
			return fmt.Errorf("kube-apiserver mirror pod %s audit policy = %q, want %q", pod.Name, path, auditPolicyPath)
		}
		if err := assertAuditPolicyMount(&pod); err != nil {
			return err
		}
	}
	return nil
}

func assertAuditPolicyMount(pod *corev1.Pod) error {
	var container *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "kube-apiserver" {
			container = &pod.Spec.Containers[i]
			break
		}
	}
	if container == nil {
		return fmt.Errorf("kube-apiserver mirror pod %s has no kube-apiserver container", pod.Name)
	}

	mounted := false
	for _, mount := range container.VolumeMounts {
		if mount.Name == auditPolicyVolumeName && mount.MountPath == "/etc/kubernetes/policies" && mount.ReadOnly {
			mounted = true
			break
		}
	}
	if !mounted {
		return fmt.Errorf("kube-apiserver mirror pod %s is missing the read-only audit policy mount", pod.Name)
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == auditPolicyVolumeName && volume.HostPath != nil && volume.HostPath.Path == "/etc/kubernetes/policies" {
			return nil
		}
	}
	return fmt.Errorf("kube-apiserver mirror pod %s is missing the audit policy hostPath", pod.Name)
}

func assertSystemReservedMemory(ctx context.Context, tenantClient client.Client, spec nke.ClusterSpec) error {
	expected, err := expectedNodeMemoryReservation(spec)
	if err != nil {
		return err
	}

	var nodes corev1.NodeList
	if err := tenantClient.List(ctx, &nodes); err != nil {
		return fmt.Errorf("listing tenant nodes: %w", err)
	}
	if want := clusterNodeCount(spec); len(nodes.Items) != want {
		return fmt.Errorf("found %d tenant nodes while checking memory reservation, want %d", len(nodes.Items), want)
	}
	for _, node := range nodes.Items {
		if ready, detail := kubernetesNodeReady(&node); !ready {
			return fmt.Errorf("tenant node %s is not Ready while checking memory reservation: %s", node.Name, detail)
		}
		capacity, capacityFound := node.Status.Capacity[corev1.ResourceMemory]
		allocatable, allocatableFound := node.Status.Allocatable[corev1.ResourceMemory]
		if !capacityFound || !allocatableFound {
			return fmt.Errorf("tenant node %s does not report memory capacity and allocatable values", node.Name)
		}
		reservedBytes := capacity.Value() - allocatable.Value()
		if reservedBytes < expected.Value() {
			return fmt.Errorf("tenant node %s reserves %d memory bytes, want at least %s from kubeReserved + systemReserved", node.Name, reservedBytes, expected.String())
		}
	}
	return nil
}

func expectedNodeMemoryReservation(spec nke.ClusterSpec) (resource.Quantity, error) {
	systemReserved := ""
	pools := slices.Concat(spec.ControlPlane.Pools, spec.Workers)
	if cp := spec.ControlPlane.Config; cp != nil {
		pools = append(pools, nke.NodePoolSpec{Name: "control-plane", SystemReservedMemory: cp.SystemReservedMemory})
	}
	for _, pool := range pools {
		if systemReserved == "" {
			systemReserved = pool.SystemReservedMemory
			continue
		}
		if pool.SystemReservedMemory != systemReserved {
			return resource.Quantity{}, fmt.Errorf("post-ready memory assertion requires one reservation across the CAPD topology, got %q and %q", systemReserved, pool.SystemReservedMemory)
		}
	}
	if systemReserved == "" {
		return resource.Quantity{}, fmt.Errorf("CAPD topology has no system-reserved memory setting")
	}

	expected, err := resource.ParseQuantity(kubeReservedMemory)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("parsing fixed kube-reserved memory %q: %w", kubeReservedMemory, err)
	}
	system, err := resource.ParseQuantity(systemReserved)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("parsing system-reserved memory %q: %w", systemReserved, err)
	}
	expected.Add(system)
	return expected, nil
}

func containerFlagValue(containers []corev1.Container, containerName, flagName string) (string, error) {
	for _, container := range containers {
		if container.Name != containerName {
			continue
		}
		args := append(append([]string{}, container.Command...), container.Args...)
		flag := "--" + flagName
		for i, arg := range args {
			if value, ok := strings.CutPrefix(arg, flag+"="); ok && value != "" {
				return value, nil
			}
			if arg == flag && i+1 < len(args) && args[i+1] != "" {
				return args[i+1], nil
			}
		}
		return "", fmt.Errorf("flag %s is missing from container %s", flag, containerName)
	}
	return "", fmt.Errorf("container %s is missing", containerName)
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
