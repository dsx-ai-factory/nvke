// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/kube"
)

const (
	ovnPolicyNamespaceLabel     = "e2e.nvidia.com/ovn-policy-namespace"
	ovnPolicyNamespaceNameLabel = "kubernetes.io/metadata.name"
	ovnPolicyRoleLabel          = "e2e.nvidia.com/ovn-policy-role"
	ovnPolicyTargetRole         = "target"
	ovnPolicyControlRole        = "control"
	ovnPolicyDeniedSourceRole   = "denied-source"
	ovnPolicyAllowedSourceRole  = "allowed-source"

	ovnPolicyProbeTimeout     = 3 * time.Minute
	ovnPolicyConvergenceLimit = 2 * time.Minute
)

func validateOVNNetworkPolicyPostCreate(test *tenantTest) {
	It("enforces isolated cross-node ingress and egress NetworkPolicies", func(ctx context.Context) {
		tenantClient := expectSucceed(test.tenantClients.GetAdminClient(ctx, test.cluster.Name))
		sourceNode, targetNode, err := ovnPolicyNodes(ctx, tenantClient)
		Expect(err).NotTo(HaveOccurred())

		sourceNamespace := createOVNPolicyNamespace(ctx, tenantClient, "source")
		allowedNamespace := createOVNPolicyNamespace(ctx, tenantClient, "allowed")
		targetNamespace := createOVNPolicyNamespace(ctx, tenantClient, "target")
		target := createOVNPolicyServer(ctx, tenantClient, targetNamespace.Name, targetNode, ovnPolicyTargetRole)
		control := createOVNPolicyServer(ctx, tenantClient, targetNamespace.Name, targetNode, ovnPolicyControlRole)
		targetURL := podCIDRURL(target.Status.PodIP, podCIDRServerPort)
		controlURL := podCIDRURL(control.Status.PodIP, podCIDRServerPort)

		By("establishing a cross-node connectivity baseline")
		Expect(runOVNPolicyProbe(ctx, tenantClient, sourceNamespace.Name, sourceNode, ovnPolicyDeniedSourceRole, targetURL, "", true)).To(Succeed())

		ingress := ovnIngressPolicy(targetNamespace.Name, allowedNamespace.Name)
		Expect(tenantClient.Create(ctx, ingress)).To(Succeed())
		deferDeletePodCIDRObjects(ctx, tenantClient, ingress)

		By("denying an isolated source at ingress while its control path stays reachable")
		expectOVNPolicyDenialEventually(ctx, tenantClient, sourceNamespace.Name, sourceNode, ovnPolicyDeniedSourceRole, targetURL, controlURL)
		Expect(runOVNPolicyProbe(ctx, tenantClient, allowedNamespace.Name, sourceNode, ovnPolicyAllowedSourceRole, targetURL, "", true)).To(Succeed())

		Expect(tenantClient.Delete(ctx, ingress)).To(Succeed())
		By("restoring ingress after the policy is removed")
		expectOVNPolicyReachableEventually(ctx, tenantClient, sourceNamespace.Name, sourceNode, ovnPolicyDeniedSourceRole, targetURL)

		egress := ovnEgressPolicy(sourceNamespace.Name, targetNamespace.Name)
		Expect(tenantClient.Create(ctx, egress)).To(Succeed())
		deferDeletePodCIDRObjects(ctx, tenantClient, egress)

		By("denying egress to the target while retaining the selected control path")
		expectOVNPolicyDenialEventually(ctx, tenantClient, sourceNamespace.Name, sourceNode, ovnPolicyDeniedSourceRole, targetURL, controlURL)
		Expect(runOVNPolicyProbe(ctx, tenantClient, allowedNamespace.Name, sourceNode, ovnPolicyAllowedSourceRole, targetURL, "", true)).To(Succeed())

		Expect(tenantClient.Delete(ctx, egress)).To(Succeed())
		By("restoring egress after the policy is removed")
		expectOVNPolicyReachableEventually(ctx, tenantClient, sourceNamespace.Name, sourceNode, ovnPolicyDeniedSourceRole, targetURL)
	}, SpecTimeout(12*time.Minute))
}

func createOVNPolicyNamespace(ctx context.Context, tenantClient client.Client, role string) *corev1.Namespace {
	GinkgoHelper()
	namespace := ovnPolicyNamespace(role)
	Expect(tenantClient.Create(ctx, namespace)).To(Succeed())
	deferDeletePodCIDRObjects(ctx, tenantClient, namespace)
	return namespace
}

func ovnPolicyNamespace(role string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		GenerateName: "ovn-policy-" + role + "-",
		Labels:       map[string]string{ovnPolicyNamespaceLabel: role},
	}}
}

type ovnPolicyNode struct {
	name     string
	hostname string
}

func createOVNPolicyServer(ctx context.Context, tenantClient client.Client, namespace string, node ovnPolicyNode, role string) *corev1.Pod {
	GinkgoHelper()
	pod := podCIDRServerPod(role, node.hostname, false, podCIDRServerPort)
	pod.Namespace = namespace
	pod.Labels = map[string]string{ovnPolicyRoleLabel: role}
	Expect(tenantClient.Create(ctx, pod)).To(Succeed())
	deferDeletePodCIDRObjects(ctx, tenantClient, pod)
	ready := waitForPodReady(ctx, tenantClient, client.ObjectKeyFromObject(pod))
	Expect(ready.Spec.NodeName).To(Equal(node.name), "server should run on the selected cross-node target")
	return ready
}

func ovnPolicyNodes(ctx context.Context, tenantClient client.Client) (ovnPolicyNode, ovnPolicyNode, error) {
	var nodes corev1.NodeList
	if err := tenantClient.List(ctx, &nodes); err != nil {
		return ovnPolicyNode{}, ovnPolicyNode{}, fmt.Errorf("listing tenant nodes: %w", err)
	}
	readyNodes := make([]ovnPolicyNode, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue && node.Labels[corev1.LabelHostname] != "" {
				readyNodes = append(readyNodes, ovnPolicyNode{name: node.Name, hostname: node.Labels[corev1.LabelHostname]})
				break
			}
		}
	}
	if len(readyNodes) < 2 {
		return ovnPolicyNode{}, ovnPolicyNode{}, fmt.Errorf("need at least two Ready tenant nodes for cross-node NetworkPolicy checks, found %d", len(readyNodes))
	}
	return readyNodes[0], readyNodes[1], nil
}

func ovnIngressPolicy(targetNamespace, allowedNamespace string) *networkingv1.NetworkPolicy {
	protocol := corev1.ProtocolTCP
	port := intstr.FromInt32(podCIDRServerPort)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "ovn-policy-ingress-", Namespace: targetNamespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{ovnPolicyRoleLabel: ovnPolicyTargetRole}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{ovnPolicyNamespaceNameLabel: allowedNamespace}}}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protocol, Port: &port}},
			}},
		},
	}
}

func ovnEgressPolicy(sourceNamespace, targetNamespace string) *networkingv1.NetworkPolicy {
	protocol := corev1.ProtocolTCP
	port := intstr.FromInt32(podCIDRServerPort)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "ovn-policy-egress-", Namespace: sourceNamespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{ovnPolicyRoleLabel: ovnPolicyDeniedSourceRole}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{ovnPolicyNamespaceNameLabel: targetNamespace}},
					PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{ovnPolicyRoleLabel: ovnPolicyControlRole}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protocol, Port: &port}},
			}},
		},
	}
}

func expectOVNPolicyReachableEventually(ctx context.Context, tenantClient client.Client, namespace string, node ovnPolicyNode, role, targetURL string) {
	GinkgoHelper()
	Eventually(func(probeCtx context.Context) error {
		return runOVNPolicyProbe(probeCtx, tenantClient, namespace, node, role, targetURL, "", true)
	}).WithContext(ctx).WithTimeout(ovnPolicyConvergenceLimit).WithPolling(5 * time.Second).Should(Succeed())
}

func expectOVNPolicyDenialEventually(ctx context.Context, tenantClient client.Client, namespace string, node ovnPolicyNode, role, targetURL, controlURL string) {
	GinkgoHelper()
	Eventually(func(probeCtx context.Context) error {
		return runOVNPolicyProbe(probeCtx, tenantClient, namespace, node, role, targetURL, controlURL, false)
	}).WithContext(ctx).WithTimeout(ovnPolicyConvergenceLimit).WithPolling(5 * time.Second).Should(Succeed())
}

func runOVNPolicyProbe(ctx context.Context, tenantClient client.Client, namespace string, node ovnPolicyNode, role, targetURL, controlURL string, reachable bool) error {
	pod := podCIDRProbePod(role, node.hostname, false, nil)
	pod.Namespace = namespace
	pod.Labels = map[string]string{ovnPolicyRoleLabel: role}
	pod.Spec.Containers[0].Command = []string{"sh", "-c", ovnPolicyProbeScript(reachable), "probe", targetURL, controlURL}
	pod.Spec.Containers[0].Args = nil
	finished, message, err := kube.RunPod(ctx, tenantClient, pod, podCIDRProbeContainer, ovnPolicyProbeTimeout)
	if err != nil {
		return fmt.Errorf("policy probe %s from %s/%s to %s: %w", map[bool]string{true: "reachability", false: "denial"}[reachable], namespace, role, targetURL, err)
	}
	if message == "" {
		return fmt.Errorf("policy probe %s from %s/%s reported no result", map[bool]string{true: "reachability", false: "denial"}[reachable], namespace, role)
	}
	if finished.Spec.NodeName != node.name {
		return fmt.Errorf("policy probe %s from %s/%s ran on node %q, want %q", map[bool]string{true: "reachability", false: "denial"}[reachable], namespace, role, finished.Spec.NodeName, node.name)
	}
	return nil
}

func ovnPolicyProbeScript(reachable bool) string {
	if reachable {
		return `set -u
if ! command -v curl >/dev/null 2>&1; then
  echo 'curl is required for the policy probe' > /dev/termination-log
  exit 127
fi
curl -fsS --connect-timeout 2 --max-time 5 --retry 0 -o /dev/null "$1"
status=$?
if [ "${status}" -eq 0 ]; then
  printf 'curl exit %s as expected\n' "${status}" > /dev/termination-log
  exit 0
fi
printf 'curl exit %s, want 0\n' "${status}" > /dev/termination-log
exit 1
`
	}
	return `set -u
if ! command -v curl >/dev/null 2>&1; then
  echo 'curl is required for the policy probe' > /dev/termination-log
  exit 127
fi
curl -fsS --connect-timeout 2 --max-time 5 --retry 0 -o /dev/null "$2"
control_before=$?
curl -fsS --connect-timeout 2 --max-time 5 --retry 0 -o /dev/null "$1"
target=$?
curl -fsS --connect-timeout 2 --max-time 5 --retry 0 -o /dev/null "$2"
control_after=$?
if [ "${control_before}" -ne 0 ] || [ "${control_after}" -ne 0 ]; then
  printf 'control curl exits before=%s after=%s, want 0\n' "${control_before}" "${control_after}" > /dev/termination-log
  exit 1
fi
if [ "${target}" -eq 28 ]; then
  printf 'target curl exit %s with control reachable before and after as expected\n' "${target}" > /dev/termination-log
  exit 0
fi
printf 'target curl exit %s, want 28\n' "${target}" > /dev/termination-log
exit 1
`
}

func TestOVNIngressPolicyAllowsOnlyTheAllowedSource(t *testing.T) {
	policy := ovnIngressPolicy("target", "allowed")
	if policy.Namespace != "target" {
		t.Fatalf("Namespace = %q, want target", policy.Namespace)
	}
	if policy.Spec.PodSelector.MatchLabels[ovnPolicyRoleLabel] != ovnPolicyTargetRole {
		t.Fatalf("PodSelector = %#v, want target role", policy.Spec.PodSelector.MatchLabels)
	}
	if len(policy.Spec.PolicyTypes) != 1 || policy.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Fatalf("PolicyTypes = %#v, want ingress only", policy.Spec.PolicyTypes)
	}
	if len(policy.Spec.Ingress) != 1 || len(policy.Spec.Ingress[0].From) != 1 || len(policy.Spec.Ingress[0].Ports) != 1 {
		t.Fatalf("Ingress = %#v, want one source and port", policy.Spec.Ingress)
	}
	peer := policy.Spec.Ingress[0].From[0]
	if peer.NamespaceSelector == nil || peer.NamespaceSelector.MatchLabels[ovnPolicyNamespaceNameLabel] != "allowed" {
		t.Fatalf("NamespaceSelector = %#v, want allowed namespace", peer.NamespaceSelector)
	}
	assertPolicyPort(t, policy.Spec.Ingress[0].Ports[0])
}

func TestOVNEgressPolicyKeepsControlReachable(t *testing.T) {
	policy := ovnEgressPolicy("source", "target")
	if policy.Namespace != "source" {
		t.Fatalf("Namespace = %q, want source", policy.Namespace)
	}
	if policy.Spec.PodSelector.MatchLabels[ovnPolicyRoleLabel] != ovnPolicyDeniedSourceRole {
		t.Fatalf("PodSelector = %#v, want denied source role", policy.Spec.PodSelector.MatchLabels)
	}
	if len(policy.Spec.PolicyTypes) != 1 || policy.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Fatalf("PolicyTypes = %#v, want egress only", policy.Spec.PolicyTypes)
	}
	if len(policy.Spec.Egress) != 1 || len(policy.Spec.Egress[0].To) != 1 || len(policy.Spec.Egress[0].Ports) != 1 {
		t.Fatalf("Egress = %#v, want one control destination and port", policy.Spec.Egress)
	}
	peer := policy.Spec.Egress[0].To[0]
	if peer.NamespaceSelector == nil || peer.NamespaceSelector.MatchLabels[ovnPolicyNamespaceNameLabel] != "target" {
		t.Fatalf("NamespaceSelector = %#v, want target namespace", peer.NamespaceSelector)
	}
	if peer.PodSelector == nil || peer.PodSelector.MatchLabels[ovnPolicyRoleLabel] != ovnPolicyControlRole {
		t.Fatalf("PodSelector = %#v, want control role", peer.PodSelector)
	}
	assertPolicyPort(t, policy.Spec.Egress[0].Ports[0])
}

func TestOVNPolicyProbeScriptsClassifyExpectedNetworkDenial(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		reachable   bool
		targetExit  int
		controlExit int
		curlPresent bool
		wantExit    int
	}{
		{
			name:        "reachable accepts curl success",
			reachable:   true,
			targetExit:  0,
			curlPresent: true,
			wantExit:    0,
		},
		{
			name:        "denied accepts target timeout with control reachable before and after",
			reachable:   false,
			targetExit:  28,
			controlExit: 0,
			curlPresent: true,
			wantExit:    0,
		},
		{
			name:        "denied rejects an unavailable control path",
			reachable:   false,
			targetExit:  28,
			controlExit: 28,
			curlPresent: true,
			wantExit:    1,
		},
		{
			name:        "denied rejects target reachability",
			reachable:   false,
			targetExit:  0,
			controlExit: 0,
			curlPresent: true,
			wantExit:    1,
		},
		{
			name:        "denied rejects a missing curl command",
			reachable:   false,
			curlPresent: false,
			wantExit:    127,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			exitCode, terminationMessage := runMockOVNPolicyProbe(t, ovnPolicyProbeScript(tt.reachable), tt.targetExit, tt.controlExit, tt.curlPresent)
			if exitCode != tt.wantExit {
				t.Fatalf("script exit = %d, want %d; termination message: %q", exitCode, tt.wantExit, terminationMessage)
			}
			if terminationMessage == "" {
				t.Fatal("script did not write a termination message")
			}
		})
	}
}

func runMockOVNPolicyProbe(t *testing.T, script string, targetExit, controlExit int, curlPresent bool) (int, string) {
	t.Helper()
	directory := t.TempDir()
	binDirectory := filepath.Join(directory, "bin")
	if err := os.Mkdir(binDirectory, 0o755); err != nil {
		t.Fatalf("creating mock command directory: %v", err)
	}
	if curlPresent {
		// A shell function avoids executing a freshly written mock file, which
		// intermittently fails with ETXTBSY on Linux. See https://github.com/dsx-ai-factory/nke/issues/1265.
		script = fmt.Sprintf(`curl() {
for arg; do
  url="${arg}"
done
case "${url}" in
  http://target) return %d ;;
  http://control) return %d ;;
  *) return 99 ;;
esac
}
`, targetExit, controlExit) + script
	}
	terminationLog := filepath.Join(directory, "termination.log")
	script = strings.ReplaceAll(script, "/dev/termination-log", terminationLog)
	command := exec.CommandContext(t.Context(), "/bin/sh", "-c", script, "probe", "http://target", "http://control")
	command.Env = []string{"PATH=" + binDirectory}
	output, err := command.CombinedOutput()
	if len(output) > 0 {
		t.Logf("probe stdout/stderr: %s", output)
	}
	terminationMessage, readErr := os.ReadFile(terminationLog)
	if readErr != nil {
		t.Fatalf("reading termination message: %v", readErr)
	}
	if err == nil {
		return 0, string(terminationMessage)
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("running probe script: %v", err)
	}
	return exitError.ExitCode(), string(terminationMessage)
}

func assertPolicyPort(t *testing.T, port networkingv1.NetworkPolicyPort) {
	t.Helper()
	if port.Protocol == nil || *port.Protocol != corev1.ProtocolTCP {
		t.Fatalf("Protocol = %v, want TCP", port.Protocol)
	}
	if port.Port == nil || port.Port.Type != intstr.Int || port.Port.IntVal != podCIDRServerPort {
		t.Fatalf("Port = %#v, want TCP/%d", port.Port, podCIDRServerPort)
	}
}

func TestOVNPolicyNamespaceCarriesItsSelectorLabel(t *testing.T) {
	namespace := ovnPolicyNamespace("source")
	if namespace.Labels[ovnPolicyNamespaceLabel] != "source" {
		t.Fatalf("Labels = %#v, want %s=source", namespace.Labels, ovnPolicyNamespaceLabel)
	}
	if namespace.GenerateName == "" || namespace.Name != "" {
		t.Fatalf("ObjectMeta = %#v, want generated namespace name", namespace.ObjectMeta)
	}
}

func TestOVNPolicySelectorsMatchCreatedNamespaceNames(t *testing.T) {
	allowedNamespace := ovnPolicyNamespace("allowed")
	allowedNamespace.Name = "ovn-policy-allowed-7f7bc8d6c7"
	targetNamespace := ovnPolicyNamespace("target")
	targetNamespace.Name = "ovn-policy-target-cb6b98dcb6"
	ingress := ovnIngressPolicy("target", allowedNamespace.Name)
	selector, err := metav1.LabelSelectorAsSelector(ingress.Spec.Ingress[0].From[0].NamespaceSelector)
	if err != nil {
		t.Fatalf("building ingress namespace selector: %v", err)
	}
	if !selector.Matches(klabels.Set{ovnPolicyNamespaceNameLabel: allowedNamespace.Name}) {
		t.Fatalf("selector %q does not match generated namespace %q", selector, allowedNamespace.Name)
	}
	if selector.Matches(klabels.Set{ovnPolicyNamespaceLabel: "allowed"}) {
		t.Fatalf("selector %q incorrectly matches the test-only role label", selector)
	}
	egress := ovnEgressPolicy("source", targetNamespace.Name)
	selector, err = metav1.LabelSelectorAsSelector(egress.Spec.Egress[0].To[0].NamespaceSelector)
	if err != nil {
		t.Fatalf("building egress namespace selector: %v", err)
	}
	if !selector.Matches(klabels.Set{ovnPolicyNamespaceNameLabel: targetNamespace.Name}) {
		t.Fatalf("selector %q does not match generated namespace %q", selector, targetNamespace.Name)
	}
}
