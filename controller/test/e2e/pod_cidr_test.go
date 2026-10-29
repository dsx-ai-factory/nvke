// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

const (
	ovnNodeSubnetsAnnotation = "k8s.ovn.org/node-subnets"
	podCIDRServerLabel       = "e2e.nvidia.com/pod-cidr-server"
	podCIDRServerContainer   = "server"
	podCIDRProbeContainer    = "probe"
	podCIDRPrimaryService    = "pod-cidr-primary"
	podCIDRServerPort        = 8080
	podCIDRHostServerPort    = 18080

	podCIDRProbeScript = `set -eu
for target in "$@"; do
  attempt=0
  until wget -q -T 2 -O /dev/null "$target"; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 30 ]; then
      echo "unreachable $target" > /dev/termination-log
      exit 1
    fi
    sleep 2
  done
done
printf 'reachable %s\n' "$*" > /dev/termination-log
`
)

// validatePodCIDRPostCreate checks explicit multiple pod CIDR specs. Other
// cluster permutations skip this validation because they do not encode the
// node-subnet rollover contract exercised here.
func validatePodCIDRPostCreate(test *tenantTest) {
	It("assigns routable pod IPs from the configured pod CIDR ranges", func(ctx context.Context) {
		if test.spec.Network == nil || len(test.spec.Network.ExpectedPodCIDRs) < 2 {
			Skip("cluster spec does not configure multiple pod CIDR expectations")
		}

		expectedPrefixes := expectSucceed(parseExpectedPodCIDRs(test.spec.Network.ExpectedPodCIDRs))

		By("Asserting the controller reports the requested network contract")
		Eventually(func(ctx context.Context) error {
			resp, err := test.nvke.API().GetClusterWithResponse(ctx, test.cluster.ID)
			if err != nil {
				return fmt.Errorf("getting cluster %s: %w", test.cluster.ID, err)
			}
			if resp.StatusCode() != 200 || resp.JSON200 == nil {
				return fmt.Errorf("getting cluster %s: unexpected status %s: %s", test.cluster.ID, resp.Status(), resp.Body)
			}

			network := resp.JSON200.Network
			actualPodCIDR, err := canonicalPodCIDRs(network.PodCIDR)
			if err != nil {
				return fmt.Errorf("parsing controller podCIDR %q: %w", network.PodCIDR, err)
			}
			expectedPodCIDR, err := canonicalPodCIDRs(test.spec.Network.PodCIDR)
			if err != nil {
				return fmt.Errorf("parsing expected podCIDR %q: %w", test.spec.Network.PodCIDR, err)
			}
			if actualPodCIDR != expectedPodCIDR {
				return fmt.Errorf("controller podCIDR = %q, want %q", network.PodCIDR, test.spec.Network.PodCIDR)
			}
			if test.spec.Network.ServiceCIDR != "" && network.ServiceCIDR != test.spec.Network.ServiceCIDR {
				return fmt.Errorf("controller serviceCIDR = %q, want %q", network.ServiceCIDR, test.spec.Network.ServiceCIDR)
			}
			return nil
		}).WithContext(ctx).WithTimeout(1 * time.Minute).WithPolling(1 * time.Second).Should(Succeed())

		test.log.Info("Building an admin client for pod CIDR connectivity probes")
		var tenantClient client.Client
		Eventually(func(ctx context.Context) error {
			var err error
			tenantClient, err = test.tenantClients.GetAdminClient(ctx, test.cluster.Name)
			return err
		}).WithContext(ctx).WithTimeout(1 * time.Minute).WithPolling(1 * time.Second).Should(Succeed())

		validateMultiPodCIDRConnectivity(ctx, test, tenantClient, expectedPrefixes)
	}, Label("pod-cidr"), SpecTimeout(45*time.Minute))
}

func validateMultiPodCIDRConnectivity(ctx context.Context, test *tenantTest, tenantClient client.Client, expectedPrefixes []netip.Prefix) {
	GinkgoHelper()

	By("Waiting for every desired tenant node to be Kubernetes Ready")
	expectedPools := expectedNodeCounts(test.spec)
	var readyNodeNames []string
	Eventually(func(ctx context.Context) error {
		var err error
		readyNodeNames, err = validateNodesReadyInCluster(ctx, test, tenantClient, expectedPools)
		return err
	}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(2*time.Second).
		Should(Succeed(), "all desired nodes should become ready before pod CIDR validation")
	test.log.Info("All desired tenant nodes are ready for pod CIDR validation", "nodes", readyNodeNames)

	By("Waiting for OVN to exhaust the primary range and allocate a secondary-range node subnet")
	var primaryNodes, secondaryNodes []corev1.Node
	Eventually(func(ctx context.Context) error {
		var nodes corev1.NodeList
		if err := tenantClient.List(ctx, &nodes); err != nil {
			return fmt.Errorf("listing tenant nodes: %w", err)
		}
		var err error
		primaryNodes, secondaryNodes, err = classifyPodCIDRNodes(nodes.Items, expectedPrefixes)
		if err != nil {
			return err
		}
		primaryCapacity := 1 << (24 - expectedPrefixes[0].Bits())
		if len(primaryNodes) != primaryCapacity {
			return fmt.Errorf("primary pod CIDR has %d allocated node subnets, want exhausted capacity %d", len(primaryNodes), primaryCapacity)
		}
		if len(secondaryNodes) == 0 {
			return fmt.Errorf("no node subnet was allocated from secondary pod CIDRs")
		}
		return nil
	}).WithContext(ctx).WithTimeout(5 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())

	primaryNode := primaryNodes[0]
	secondaryNode := secondaryNodes[0]
	primaryNodeHostname := primaryNode.Labels[corev1.LabelHostname]
	secondaryNodeHostname := secondaryNode.Labels[corev1.LabelHostname]
	Expect(primaryNodeHostname).NotTo(BeEmpty(), "primary node %q is missing label %q", primaryNode.Name, corev1.LabelHostname)
	Expect(secondaryNodeHostname).NotTo(BeEmpty(), "secondary node %q is missing label %q", secondaryNode.Name, corev1.LabelHostname)
	primaryNodeIP := expectSucceed(nodeInternalIP(&primaryNode))
	test.log.Info("Validated OVN node-subnet rollover", "primaryNode", primaryNode.Name, "secondaryNode", secondaryNode.Name, "primaryNodeSubnets", len(primaryNodes), "secondaryNodeSubnets", len(secondaryNodes))

	By("Starting pod-network and host-network servers on the selected nodes")
	primaryServer := podCIDRServerPod("primary", primaryNodeHostname, false, podCIDRServerPort)
	secondaryServer := podCIDRServerPod("secondary", secondaryNodeHostname, false, podCIDRServerPort)
	hostServer := podCIDRServerPod("host", primaryNodeHostname, true, podCIDRHostServerPort)
	deferDeletePodCIDRObjects(ctx, tenantClient, primaryServer, secondaryServer, hostServer)
	for _, pod := range []*corev1.Pod{primaryServer, secondaryServer, hostServer} {
		Expect(tenantClient.Create(ctx, pod)).To(Succeed())
	}
	primaryServer = waitForPodReady(ctx, tenantClient, client.ObjectKeyFromObject(primaryServer))
	secondaryServer = waitForPodReady(ctx, tenantClient, client.ObjectKeyFromObject(secondaryServer))
	hostServer = waitForPodReady(ctx, tenantClient, client.ObjectKeyFromObject(hostServer))
	expectPodInCIDRs(primaryServer, primaryNode.Name, expectedPrefixes[:1])
	expectPodInCIDRs(secondaryServer, secondaryNode.Name, expectedPrefixes[1:])
	Expect(hostServer.Spec.NodeName).To(Equal(primaryNode.Name))

	service := podCIDRService()
	Expect(tenantClient.Create(ctx, service)).To(Succeed())
	deferDeletePodCIDRObjects(ctx, tenantClient, service)

	By("Checking same-node, cross-node, Service, and pod-to-node connectivity from the secondary range")
	secondaryProbe := podCIDRProbePod("secondary", secondaryNodeHostname, false, []string{
		podCIDRURL(secondaryServer.Status.PodIP, podCIDRServerPort),
		podCIDRURL(primaryServer.Status.PodIP, podCIDRServerPort),
		podCIDRURL(podCIDRPrimaryService, podCIDRServerPort),
		podCIDRURL(primaryNodeIP, podCIDRHostServerPort),
	})
	finishedSecondaryProbe, secondaryMessage, err := kube.RunPod(ctx, tenantClient, secondaryProbe, podCIDRProbeContainer, 3*time.Minute)
	Expect(err).NotTo(HaveOccurred())
	expectPodInCIDRs(finishedSecondaryProbe, secondaryNode.Name, expectedPrefixes[1:])

	By("Checking reverse cross-node connectivity into the secondary range")
	primaryProbe := podCIDRProbePod("primary", primaryNodeHostname, false, []string{
		podCIDRURL(secondaryServer.Status.PodIP, podCIDRServerPort),
	})
	finishedPrimaryProbe, primaryMessage, err := kube.RunPod(ctx, tenantClient, primaryProbe, podCIDRProbeContainer, 3*time.Minute)
	Expect(err).NotTo(HaveOccurred())
	expectPodInCIDRs(finishedPrimaryProbe, primaryNode.Name, expectedPrefixes[:1])

	By("Checking node-network connectivity into the secondary range")
	hostProbe := podCIDRProbePod("host", primaryNodeHostname, true, []string{
		podCIDRURL(secondaryServer.Status.PodIP, podCIDRServerPort),
	})
	finishedHostProbe, hostMessage, err := kube.RunPod(ctx, tenantClient, hostProbe, podCIDRProbeContainer, 3*time.Minute)
	Expect(err).NotTo(HaveOccurred())
	Expect(finishedHostProbe.Spec.NodeName).To(Equal(primaryNode.Name))

	test.log.Info("Validated secondary pod CIDR allocation and connectivity", "secondaryNode", secondaryNode.Name, "secondaryPodIP", finishedSecondaryProbe.Status.PodIP, "secondaryChecks", secondaryMessage, "primaryChecks", primaryMessage, "hostChecks", hostMessage)
}

func podCIDRProbePod(role, nodeHostname string, hostNetwork bool, targets []string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pod-cidr-" + role + "-probe-",
			Namespace:    "default",
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			HostNetwork:   hostNetwork,
			NodeSelector:  map[string]string{corev1.LabelHostname: nodeHostname},
			Tolerations:   podCIDRTolerations(),
			Containers: []corev1.Container{
				{
					Name:            podCIDRProbeContainer,
					Image:           kube.KubectlImage,
					ImagePullPolicy: corev1.PullIfNotPresent,
				},
			},
		},
	}
	if hostNetwork {
		pod.Spec.DNSPolicy = corev1.DNSClusterFirstWithHostNet
	}
	pod.Spec.Containers[0].Command = []string{"sh", "-c", podCIDRProbeScript, "probe"}
	pod.Spec.Containers[0].Args = targets
	return pod
}

func podCIDRServerPod(role, nodeHostname string, hostNetwork bool, port int32) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pod-cidr-" + role + "-server-",
			Namespace:    "default",
			Labels:       map[string]string{podCIDRServerLabel: role},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			HostNetwork:   hostNetwork,
			NodeSelector:  map[string]string{corev1.LabelHostname: nodeHostname},
			Tolerations:   podCIDRTolerations(),
			Containers: []corev1.Container{
				{
					Name:            podCIDRServerContainer,
					Image:           kube.KubectlImage,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command: []string{"sh", "-c", fmt.Sprintf(
						`set -eu
cat > /tmp/respond <<'EOF'
#!/bin/sh
while IFS= read -r line; do
  [ "$line" = "$(printf '\r')" ] && break
done
printf 'HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok'
EOF
chmod +x /tmp/respond
exec nc -lk -p %d -e /tmp/respond`, port,
					)},
				},
			},
		},
	}
	if hostNetwork {
		pod.Spec.DNSPolicy = corev1.DNSClusterFirstWithHostNet
	}
	return pod
}

func podCIDRService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: podCIDRPrimaryService, Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{podCIDRServerLabel: "primary"},
			Ports: []corev1.ServicePort{
				{Port: podCIDRServerPort, TargetPort: intstr.FromInt32(podCIDRServerPort)},
			},
		},
	}
}

func podCIDRTolerations() []corev1.Toleration {
	return []corev1.Toleration{
		{Key: "node-role.kubernetes.io/control-plane", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		{Key: "node-role.kubernetes.io/master", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	}
}

func waitForPodReady(ctx context.Context, tenantClient client.Client, key client.ObjectKey) *corev1.Pod {
	GinkgoHelper()
	var pod corev1.Pod
	Eventually(func(ctx context.Context) error {
		if err := tenantClient.Get(ctx, key, &pod); err != nil {
			return fmt.Errorf("getting pod %s: %w", key, err)
		}
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return fmt.Errorf("pod %s terminated before becoming Ready, phase %s%s", key, pod.Status.Phase, podDetails(&pod))
		}
		if pod.Status.PodIP == "" {
			return fmt.Errorf("pod %s does not have an IP, phase %s%s", key, pod.Status.Phase, podDetails(&pod))
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				return nil
			}
		}
		return fmt.Errorf("pod %s is not Ready, phase %s%s", key, pod.Status.Phase, podDetails(&pod))
	}).WithContext(ctx).WithTimeout(5 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
	return pod.DeepCopy()
}

// podDetails is kube.PodStatusDetails as a suffix for an error.
func podDetails(pod *corev1.Pod) string {
	details := kube.PodStatusDetails(pod)
	if len(details) == 0 {
		return ""
	}
	return ": " + strings.Join(details, "; ")
}

func expectPodInCIDRs(pod *corev1.Pod, nodeName string, prefixes []netip.Prefix) {
	GinkgoHelper()
	Expect(pod.Spec.NodeName).To(Equal(nodeName), "pod %s should be placed on the scheduler-selected node", client.ObjectKeyFromObject(pod))
	addr := expectSucceed(netip.ParseAddr(pod.Status.PodIP))
	Expect(containsAddr(prefixes, addr)).To(BeTrue(), "pod %s assigned IP %s outside expected CIDRs %v", client.ObjectKeyFromObject(pod), addr, prefixes)
}

func classifyPodCIDRNodes(nodes []corev1.Node, prefixes []netip.Prefix) ([]corev1.Node, []corev1.Node, error) {
	if len(prefixes) < 2 {
		return nil, nil, fmt.Errorf("at least two pod CIDRs are required")
	}
	var primary, secondary []corev1.Node
	for i := range nodes {
		subnet, err := nodeIPv4PodSubnet(&nodes[i])
		if err != nil {
			return nil, nil, err
		}
		if subnet.Bits() != 24 {
			return nil, nil, fmt.Errorf("node %s OVN pod subnet %s is not a /24", nodes[i].Name, subnet)
		}
		switch {
		case prefixes[0].Contains(subnet.Addr()):
			primary = append(primary, nodes[i])
		case containsAddr(prefixes[1:], subnet.Addr()):
			secondary = append(secondary, nodes[i])
		default:
			return nil, nil, fmt.Errorf("node %s OVN pod subnet %s is outside expected pod CIDRs", nodes[i].Name, subnet)
		}
	}
	return primary, secondary, nil
}

func nodeIPv4PodSubnet(node *corev1.Node) (netip.Prefix, error) {
	raw := node.Annotations[ovnNodeSubnetsAnnotation]
	if raw == "" {
		return netip.Prefix{}, fmt.Errorf("node %s has no %s annotation", node.Name, ovnNodeSubnetsAnnotation)
	}
	var subnets struct {
		Default []string `json:"default"`
	}
	if err := json.Unmarshal([]byte(raw), &subnets); err != nil {
		return netip.Prefix{}, fmt.Errorf("parsing node %s %s annotation: %w", node.Name, ovnNodeSubnetsAnnotation, err)
	}
	for _, value := range subnets.Default {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("parsing node %s OVN pod subnet %q: %w", node.Name, value, err)
		}
		if prefix.Addr().Is4() {
			return prefix.Masked(), nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("node %s has no IPv4 OVN pod subnet", node.Name)
}

func nodeInternalIP(node *corev1.Node) (string, error) {
	for _, address := range node.Status.Addresses {
		addr, err := netip.ParseAddr(address.Address)
		if address.Type == corev1.NodeInternalIP && err == nil && addr.Is4() {
			return address.Address, nil
		}
	}
	return "", fmt.Errorf("node %s has no valid InternalIP", node.Name)
}

func podCIDRURL(host string, port int32) string {
	return fmt.Sprintf("http://%s:%d/", host, port)
}

func deferDeletePodCIDRObjects(ctx context.Context, tenantClient client.Client, objects ...client.Object) {
	GinkgoHelper()
	DeferCleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		for _, object := range objects {
			_ = tenantClient.Delete(cleanupCtx, object)
		}
	})
}

func parseExpectedPodCIDRs(values []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := parseExpectedPodCIDR(value)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

func canonicalPodCIDRs(value string) (string, error) {
	prefixes, err := parseExpectedPodCIDRs(strings.Split(value, ","))
	if err != nil {
		return "", err
	}
	canonical := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		canonical = append(canonical, prefix.String())
	}
	return strings.Join(canonical, ","), nil
}

func parseExpectedPodCIDR(value string) (netip.Prefix, error) {
	value = strings.TrimSpace(value)
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("parsing pod CIDR %q: %w", value, err)
	}
	return prefix.Masked(), nil
}

func containsAddr(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func TestCanonicalPodCIDRs(t *testing.T) {
	got, err := canonicalPodCIDRs("10.250.1.1/16, 10.251.2.3/16")
	if err != nil {
		t.Fatalf("canonicalPodCIDRs() error = %v", err)
	}
	if want := "10.250.0.0/16,10.251.0.0/16"; got != want {
		t.Fatalf("canonicalPodCIDRs() = %q, want %q", got, want)
	}
	if _, err := canonicalPodCIDRs("not-a-cidr"); err == nil {
		t.Fatal("canonicalPodCIDRs() error = nil, want parse error")
	}
}

func TestClassifyPodCIDRNodes(t *testing.T) {
	nodes := []corev1.Node{
		podCIDRNode("primary-a", `{"default":["10.250.0.0/24"]}`),
		podCIDRNode("primary-b", `{"default":["10.250.1.0/24"]}`),
		podCIDRNode("secondary-a", `{"default":["10.252.0.0/24"]}`),
	}
	prefixes, err := parseExpectedPodCIDRs([]string{"10.250.0.0/23", "10.252.0.0/22"})
	if err != nil {
		t.Fatalf("parseExpectedPodCIDRs() error = %v", err)
	}
	primary, secondary, err := classifyPodCIDRNodes(nodes, prefixes)
	if err != nil {
		t.Fatalf("classifyPodCIDRNodes() error = %v", err)
	}
	if len(primary) != 2 || primary[0].Name != "primary-a" || primary[1].Name != "primary-b" {
		t.Fatalf("primary nodes = %v, want primary-a and primary-b", nodeNames(primary))
	}
	if len(secondary) != 1 || secondary[0].Name != "secondary-a" {
		t.Fatalf("secondary nodes = %v, want secondary-a", nodeNames(secondary))
	}
}

func TestNodeIPv4PodSubnetRejectsMissingAnnotation(t *testing.T) {
	if _, err := nodeIPv4PodSubnet(&corev1.Node{}); err == nil {
		t.Fatal("nodeIPv4PodSubnet() error = nil, want missing annotation error")
	}
}

func TestPodCIDRProbePodUsesSchedulerNodeSelector(t *testing.T) {
	pod := podCIDRProbePod("secondary", "worker-secondary-hostname", false, []string{"http://10.252.0.3:8080"})
	if pod.Spec.NodeName != "" {
		t.Fatalf("pod.Spec.NodeName = %q, want scheduler to select the node", pod.Spec.NodeName)
	}
	if got := pod.Spec.NodeSelector[corev1.LabelHostname]; got != "worker-secondary-hostname" {
		t.Fatalf("hostname node selector = %q, want worker-secondary-hostname", got)
	}
}

func TestPodCIDRServerPodUsesSchedulerNodeSelector(t *testing.T) {
	pod := podCIDRServerPod("secondary", "worker-secondary-hostname", false, podCIDRServerPort)
	if pod.Spec.NodeName != "" {
		t.Fatalf("pod.Spec.NodeName = %q, want scheduler to select the node", pod.Spec.NodeName)
	}
	if got := pod.Spec.NodeSelector[corev1.LabelHostname]; got != "worker-secondary-hostname" {
		t.Fatalf("hostname node selector = %q, want worker-secondary-hostname", got)
	}
}

func podCIDRNode(name, annotation string) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        name,
		Annotations: map[string]string{ovnNodeSubnetsAnnotation: annotation},
	}}
}

func nodeNames(nodes []corev1.Node) []string {
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		names = append(names, node.Name)
	}
	return names
}
