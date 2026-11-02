// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	kubeAPIServerManifest       = "/etc/kubernetes/manifests/kube-apiserver.yaml"
	kubeAPIServerManifestBackup = "/etc/kubernetes/e2e-kube-apiserver.yaml"
)

type controlPlaneRouteTarget struct {
	name string
	ip   string
}

type frrBGPTable struct {
	Routes map[string][]struct {
		NextHops []struct {
			IP string `json:"ip"`
		} `json:"nexthops"`
	} `json:"routes"`
}

func validatePostReadyKubeVipRouteWithdrawal(test *tenantTest) {
	It("withdraws an unhealthy apiserver's BGP path within one minute", func(ctx context.Context) {
		tenantClient := expectSucceed(test.tenantClients.GetClient(ctx, test.cluster.Name))
		vip := strings.TrimSpace(expectSucceed(controlPlaneVIP(test)))
		bgpContainer := envOrDefault("E2E_BGP_CONTAINER_NAME", "bgp-containers")
		targets := expectSucceed(controlPlaneRouteTargets(ctx, tenantClient))
		Expect(targets).To(HaveLen(3), "the CAPD route-withdrawal scenario requires three control-plane nodes")

		expectedHops := make([]string, 0, len(targets))
		for _, target := range targets {
			expectedHops = append(expectedHops, target.ip)
		}
		target := targets[0]
		Expect(runDockerCommand(ctx, "inspect", target.name)).To(Succeed(),
			"CAPD node %s must map to a Docker container with the same name", target.name)

		By("Waiting for all control-plane nodes to advertise the API VIP")
		Eventually(func(ctx context.Context) ([]string, error) {
			return bgpNextHops(ctx, bgpContainer, vip)
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).
			Should(ConsistOf(expectedHops))

		manifestRemoved := false
		DeferCleanup(func(ctx context.Context) {
			if manifestRemoved {
				Expect(restoreKubeAPIServerManifest(ctx, target.name)).To(Succeed())
			}
		}, NodeTimeout(2*time.Minute))

		By(fmt.Sprintf("Stopping kube-apiserver on %s", target.name))
		Expect(runDockerCommand(
			ctx,
			"exec", target.name,
			"test", "!", "-e", kubeAPIServerManifestBackup,
		)).To(Succeed(), "a previous E2E run must not have left a staged apiserver manifest")
		Expect(runDockerCommand(
			ctx,
			"exec", target.name,
			"mv", kubeAPIServerManifest, kubeAPIServerManifestBackup,
		)).To(Succeed())
		manifestRemoved = true
		withdrawalDeadline := time.Now().Add(time.Minute)

		By("Verifying kube-vip withdraws only the failed node's BGP path within one minute")
		remainingHops := append([]string(nil), expectedHops[1:]...)
		withdrawalTimeout := time.Until(withdrawalDeadline)
		Expect(withdrawalTimeout).To(BeNumerically(">", 0), "apiserver shutdown consumed the one-minute withdrawal deadline")
		Eventually(func(ctx context.Context) ([]string, error) {
			return bgpNextHops(ctx, bgpContainer, vip)
		}).WithContext(ctx).WithTimeout(withdrawalTimeout).WithPolling(time.Second).
			Should(ConsistOf(remainingHops))

		// Route withdrawal is triggered when /readyz fails, which can happen while
		// kube-apiserver is still using its graceful termination period. Confirm the
		// CRI container stops without making full process exit a precondition of the
		// one-minute route-withdrawal assertion.
		By(fmt.Sprintf("Waiting for kube-apiserver on %s to stop", target.name))
		Eventually(func(ctx context.Context) error {
			return assertKubeAPIServerContainerStopped(ctx, target.name)
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(time.Second).Should(Succeed())

		By("Restoring kube-apiserver and verifying its BGP path returns")
		Expect(restoreKubeAPIServerManifest(ctx, target.name)).To(Succeed())
		manifestRemoved = false
		Eventually(func(ctx context.Context) error {
			return assertKubeAPIServerRestarted(ctx, tenantClient, target.name)
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
		Eventually(func(ctx context.Context) ([]string, error) {
			return bgpNextHops(ctx, bgpContainer, vip)
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).
			Should(ConsistOf(expectedHops))
	}, Label("lifecycle", "post-ready", "kube-vip-route-withdrawal"), NodeTimeout(10*time.Minute))
}

func controlPlaneVIP(test *tenantTest) (string, error) {
	if test.cluster.ControlPlaneVIP == nil || strings.TrimSpace(*test.cluster.ControlPlaneVIP) == "" {
		return "", fmt.Errorf("cluster %s has no control-plane VIP", test.cluster.ID)
	}
	return *test.cluster.ControlPlaneVIP, nil
}

func controlPlaneRouteTargets(ctx context.Context, tenantClient client.Client) ([]controlPlaneRouteTarget, error) {
	var nodes corev1.NodeList
	if err := tenantClient.List(ctx, &nodes, client.MatchingLabels{"node-role.kubernetes.io/control-plane": ""}); err != nil {
		return nil, fmt.Errorf("listing control-plane nodes: %w", err)
	}
	targets := make([]controlPlaneRouteTarget, 0, len(nodes.Items))
	for i := range nodes.Items {
		address := ""
		for _, candidate := range nodes.Items[i].Status.Addresses {
			if candidate.Type == corev1.NodeInternalIP {
				address = candidate.Address
				break
			}
		}
		if address == "" {
			return nil, fmt.Errorf("control-plane node %s has no InternalIP", nodes.Items[i].Name)
		}
		targets = append(targets, controlPlaneRouteTarget{name: nodes.Items[i].Name, ip: address})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].name < targets[j].name })
	return targets, nil
}

func bgpNextHops(ctx context.Context, container, vip string) ([]string, error) {
	command := exec.CommandContext(ctx,
		"docker", "exec", container,
		"vtysh", "-c", "show bgp ipv4 unicast json",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, commandError(command, err, output)
	}
	return parseBGPNextHops(output, vip+"/32")
}

func parseBGPNextHops(output []byte, prefix string) ([]string, error) {
	var table frrBGPTable
	if err := json.Unmarshal(output, &table); err != nil {
		return nil, fmt.Errorf("decoding FRR BGP table: %w", err)
	}
	paths, ok := table.Routes[prefix]
	if !ok {
		return nil, fmt.Errorf("FRR has no BGP route for %s", prefix)
	}
	seen := make(map[string]struct{})
	for _, path := range paths {
		for _, nextHop := range path.NextHops {
			if nextHop.IP != "" {
				seen[nextHop.IP] = struct{}{}
			}
		}
	}
	hops := make([]string, 0, len(seen))
	for hop := range seen {
		hops = append(hops, hop)
	}
	sort.Strings(hops)
	if len(hops) == 0 {
		return nil, fmt.Errorf("FRR route %s has no next hops", prefix)
	}
	return hops, nil
}

func runDockerCommand(ctx context.Context, args ...string) error {
	command := exec.CommandContext(ctx, "docker", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return commandError(command, err, output)
	}
	return nil
}

// nodeContainerName resolves the Docker container that backs a node. CAPD names
// the container after the node; nico-sim appends the first eight characters of
// the instance ID (controller/nico-sim/internal/container/service.go), so the
// node name is a prefix there rather than the whole name.
func nodeContainerName(ctx context.Context, nodeName string) (string, error) {
	command := exec.CommandContext(ctx,
		"docker", "ps", "--format", "{{.Names}}",
		"--filter", "name=^"+regexp.QuoteMeta(nodeName)+"(-[0-9a-f]{8})?$",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", commandError(command, err, output)
	}
	names := strings.Fields(string(output))
	if len(names) != 1 {
		return "", fmt.Errorf("expected one running container for node %s, found %v", nodeName, names)
	}
	return names[0], nil
}

func commandError(command *exec.Cmd, err error, output []byte) error {
	details := strings.TrimSpace(string(output))
	if details != "" {
		return fmt.Errorf("running %s: %w: %s", strings.Join(command.Args, " "), err, details)
	}
	return fmt.Errorf("running %s: %w", strings.Join(command.Args, " "), err)
}

func restoreKubeAPIServerManifest(ctx context.Context, container string) error {
	return runDockerCommand(
		ctx,
		"exec", container,
		"mv", kubeAPIServerManifestBackup, kubeAPIServerManifest,
	)
}

func assertKubeAPIServerContainerStopped(ctx context.Context, container string) error {
	ids, err := kubeAPIServerContainerIDs(ctx, container)
	if err != nil {
		return err
	}
	if len(ids) != 0 {
		return fmt.Errorf("kube-apiserver on %s still has running container IDs %v", container, ids)
	}
	return nil
}

func kubeAPIServerContainerIDs(ctx context.Context, container string) ([]string, error) {
	command := exec.CommandContext(
		ctx,
		"docker", "exec", container,
		"crictl", "--runtime-endpoint=unix:///run/containerd/containerd.sock",
		"ps", "--name", "kube-apiserver", "-q",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, commandError(command, err, output)
	}
	return strings.Fields(string(output)), nil
}

// assertKubeAPIServerRestarted checks that exactly one kube-apiserver container
// runs on nodeName and that its mirror pod is Ready and reports that container.
func assertKubeAPIServerRestarted(ctx context.Context, tenantClient client.Client, nodeName string) error {
	ids, err := kubeAPIServerContainerIDs(ctx, nodeName)
	if err != nil {
		return err
	}
	if len(ids) != 1 {
		return fmt.Errorf("kube-apiserver on %s has %d running container IDs, want 1: %v", nodeName, len(ids), ids)
	}
	var pods corev1.PodList
	if err := tenantClient.List(
		ctx,
		&pods,
		client.InNamespace(kubeSystemNamespace),
		client.MatchingLabels{"component": "kube-apiserver"},
	); err != nil {
		return fmt.Errorf("listing kube-apiserver mirror pods: %w", err)
	}
	for i := range pods.Items {
		if pods.Items[i].Spec.NodeName == nodeName {
			return kubeAPIServerPodReports(&pods.Items[i], ids[0])
		}
	}
	return fmt.Errorf("kube-apiserver on %s has no mirror pod", nodeName)
}

// kubeAPIServerPodReports checks that pod is Ready and that its container
// status names the CRI container containerID, such as "containerd://<id>".
func kubeAPIServerPodReports(pod *corev1.Pod, containerID string) error {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != "kube-apiserver" {
			continue
		}
		if _, id, _ := strings.Cut(status.ContainerID, "://"); id != containerID {
			return fmt.Errorf("kube-apiserver pod %s reports container %q, want running container %s", pod.Name, status.ContainerID, containerID)
		}
		if !podReady(pod) {
			return fmt.Errorf("kube-apiserver pod %s is not Ready", pod.Name)
		}
		return nil
	}
	return fmt.Errorf("kube-apiserver pod %s has no kube-apiserver container status", pod.Name)
}

func TestUnit_ParseBGPNextHops(t *testing.T) {
	input := []byte(`{
		"routes": {
			"172.31.1.1/32": [
				{"nexthops": [{"ip": "172.18.0.11"}]},
				{"nexthops": [{"ip": "172.18.0.12"}]},
				{"nexthops": [{"ip": "172.18.0.11"}]}
			]
		}
	}`)
	hops, err := parseBGPNextHops(input, "172.31.1.1/32")
	if err != nil {
		t.Fatalf("parseBGPNextHops() error = %v", err)
	}
	want := []string{"172.18.0.11", "172.18.0.12"}
	if fmt.Sprint(hops) != fmt.Sprint(want) {
		t.Fatalf("parseBGPNextHops() = %v, want %v", hops, want)
	}
}

func TestUnit_KubeAPIServerPodReports(t *testing.T) {
	pod := func(containerID string, ready corev1.ConditionStatus) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "kube-apiserver-cp-1"},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}},
				ContainerStatuses: []corev1.ContainerStatus{
					{Name: "kube-apiserver", ContainerID: containerID},
				},
			},
		}
	}
	tests := map[string]struct {
		pod     *corev1.Pod
		wantErr string
	}{
		"ready with the running container": {pod: pod("containerd://new", corev1.ConditionTrue)},
		"ready with the stopped container": {pod: pod("containerd://old", corev1.ConditionTrue), wantErr: "want running container new"},
		"running container not yet ready":  {pod: pod("containerd://new", corev1.ConditionFalse), wantErr: "is not Ready"},
		"no container status":              {pod: &corev1.Pod{}, wantErr: "has no kube-apiserver container status"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := kubeAPIServerPodReports(test.pod, "new")
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("kubeAPIServerPodReports() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("kubeAPIServerPodReports() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}
