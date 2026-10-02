// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/kube"
)

const (
	ovnLifecycleTrafficNamespacePrefix = "e2e-ovn-lifecycle-"
	ovnLifecycleTrafficServerRole      = "ovn-lifecycle"
	ovnLifecycleTrafficServiceName     = "ovn-lifecycle"
	ovnLifecycleTrafficProbeTimeout    = 2 * time.Minute
)

// validateOVNLifecycleTraffic verifies fresh pod and Service connectivity from
// every current node after a disruptive OVN lifecycle operation.
func validateOVNLifecycleTraffic(ctx context.Context, test *tenantTest, phase string, requiredNodeNames ...string) {
	GinkgoHelper()
	if test.spec.CNIPlugin != CNIPluginOVN {
		return
	}

	tenantClient := expectSucceed(test.tenantClients.GetAdminClient(ctx, test.cluster.Name))
	expectedPools := expectSucceed(currentOVNLifecycleNodeCounts(ctx, test))
	expectedNodes := totalExpectedNodes(expectedPools) + expectSucceed(currentControllerControlPlaneCount(ctx, test))
	var readyNodeNames []string
	Eventually(func(ctx context.Context) error {
		var err error
		readyNodeNames, err = validateNodesReadyInCluster(ctx, test, tenantClient, expectedPools)
		return err
	}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(2*time.Second).
		Should(Succeed(), "all current desired nodes should be Ready before OVN %s traffic validation", phase)

	for _, nodeName := range requiredNodeNames {
		Expect(readyNodeNames).To(ContainElement(nodeName), "%s traffic validation must include changed node %s", phase, nodeName)
	}
	Expect(readyNodeNames).To(HaveLen(expectedNodes), "OVN %s traffic validation should cover every current desired node", phase)
	Expect(len(readyNodeNames)).To(BeNumerically(">=", 2), "OVN %s traffic validation requires at least two nodes", phase)

	var nodes corev1.NodeList
	Expect(tenantClient.List(ctx, &nodes)).To(Succeed(), "list tenant nodes for OVN %s traffic validation", phase)
	trafficNodes := ovnLifecycleTrafficNodes(nodes.Items, readyNodeNames)

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: ovnLifecycleTrafficNamespacePrefix}}
	Expect(tenantClient.Create(ctx, namespace)).To(Succeed(), "create OVN %s traffic namespace", phase)
	defer deleteOVNLifecycleTrafficNamespace(ctx, tenantClient, namespace)

	servers := make([]*corev1.Pod, 0, len(trafficNodes))
	for _, node := range trafficNodes {
		server := ovnLifecycleTrafficServerPod(namespace.Name, node.hostname)
		Expect(tenantClient.Create(ctx, server)).To(Succeed(), "create OVN %s traffic server on %s", phase, node.name)
		server = waitForPodReady(ctx, tenantClient, client.ObjectKeyFromObject(server))
		Expect(server.Spec.NodeName).To(Equal(node.name), "OVN %s traffic server should run on %s", phase, node.name)
		servers = append(servers, server)
	}

	service := ovnLifecycleTrafficService(namespace.Name)
	Expect(tenantClient.Create(ctx, service)).To(Succeed(), "create OVN %s traffic Service", phase)
	Expect(service.Spec.ClusterIP).NotTo(Or(BeEmpty(), Equal(corev1.ClusterIPNone)), "OVN %s traffic Service should have a ClusterIP", phase)

	for i, node := range trafficNodes {
		remoteServer := servers[(i+1)%len(servers)]
		targets := []string{
			podCIDRURL(remoteServer.Status.PodIP, podCIDRServerPort),
			podCIDRURL(ovnLifecycleTrafficServiceName, podCIDRServerPort),
			podCIDRURL(service.Spec.ClusterIP, podCIDRServerPort),
		}
		probe := ovnLifecycleTrafficProbePod(namespace.Name, node.hostname, targets)
		finished, message, err := kube.RunPod(ctx, tenantClient, probe, podCIDRProbeContainer, ovnLifecycleTrafficProbeTimeout)
		Expect(err).NotTo(HaveOccurred(), "OVN %s traffic probe from %s to remote pod, DNS Service, and ClusterIP Service: %s", phase, node.name, message)
		Expect(finished.Spec.NodeName).To(Equal(node.name), "OVN %s traffic probe should run on %s", phase, node.name)
	}
}

func currentOVNLifecycleNodeCounts(ctx context.Context, test *tenantTest) (map[string]int, error) {
	pools, err := test.nke.ListNodePools(ctx, test.cluster.ID)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(pools))
	for _, pool := range pools {
		counts[pool.Name] = pool.DesiredNodeCount
	}
	return counts, nil
}

// currentControllerControlPlaneCount returns the current desired count of a
// controller-managed control plane, or zero when the control plane is a node
// pool (currentOVNLifecycleNodeCounts counts those nodes).
func currentControllerControlPlaneCount(ctx context.Context, test *tenantTest) (int, error) {
	if test.spec.ControlPlane.Config == nil {
		return 0, nil
	}
	controlPlane, err := test.nke.GetClusterControlPlane(ctx, test.cluster.ID)
	if err != nil {
		return 0, err
	}
	return controlPlane.Config.DesiredCount, nil
}

type ovnLifecycleTrafficNode struct {
	name     string
	hostname string
}

func ovnLifecycleTrafficNodes(nodes []corev1.Node, readyNodeNames []string) []ovnLifecycleTrafficNode {
	trafficNodes := make([]ovnLifecycleTrafficNode, 0, len(readyNodeNames))
	for _, nodeName := range readyNodeNames {
		nodeIndex := slices.IndexFunc(nodes, func(node corev1.Node) bool { return node.Name == nodeName })
		Expect(nodeIndex).NotTo(Equal(-1), "tenant node %s should exist for OVN lifecycle traffic validation", nodeName)
		hostname := nodes[nodeIndex].Labels[corev1.LabelHostname]
		Expect(hostname).NotTo(BeEmpty(), "tenant node %s should have hostname label for OVN lifecycle traffic validation", nodeName)
		trafficNodes = append(trafficNodes, ovnLifecycleTrafficNode{name: nodeName, hostname: hostname})
	}
	return trafficNodes
}

func ovnLifecycleTrafficServerPod(namespace, hostname string) *corev1.Pod {
	pod := podCIDRServerPod(ovnLifecycleTrafficServerRole, hostname, false, podCIDRServerPort)
	pod.Namespace = namespace
	return pod
}

func ovnLifecycleTrafficService(namespace string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: ovnLifecycleTrafficServiceName, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{podCIDRServerLabel: ovnLifecycleTrafficServerRole},
			Ports: []corev1.ServicePort{{
				Port:       podCIDRServerPort,
				TargetPort: intstr.FromInt32(podCIDRServerPort),
			}},
		},
	}
}

func ovnLifecycleTrafficProbePod(namespace, hostname string, targets []string) *corev1.Pod {
	pod := podCIDRProbePod(ovnLifecycleTrafficServerRole, hostname, false, targets)
	pod.Namespace = namespace
	return pod
}

func deleteOVNLifecycleTrafficNamespace(ctx context.Context, tenantClient client.Client, namespace *corev1.Namespace) {
	GinkgoHelper()
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	Expect(tenantClient.Delete(cleanupCtx, namespace)).To(Succeed(), "delete OVN lifecycle traffic namespace %s", namespace.Name)
	Eventually(func() error {
		var current corev1.Namespace
		err := tenantClient.Get(cleanupCtx, client.ObjectKeyFromObject(namespace), &current)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("namespace %s still exists", namespace.Name)
	}).WithContext(cleanupCtx).WithTimeout(5 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
}

func TestOVNLifecycleTrafficObjectWiring(t *testing.T) {
	const namespace = "ovn-lifecycle-test"

	service := ovnLifecycleTrafficService(namespace)
	if service.Namespace != namespace {
		t.Fatalf("service namespace = %q, want %q", service.Namespace, namespace)
	}
	if service.Name != ovnLifecycleTrafficServiceName {
		t.Fatalf("service name = %q, want %q", service.Name, ovnLifecycleTrafficServiceName)
	}
	if got := service.Spec.Selector[podCIDRServerLabel]; got != ovnLifecycleTrafficServerRole {
		t.Fatalf("service selector %q = %q, want %q", podCIDRServerLabel, got, ovnLifecycleTrafficServerRole)
	}

	server := ovnLifecycleTrafficServerPod(namespace, "node-0-hostname")
	if server.Namespace != namespace {
		t.Fatalf("server namespace = %q, want %q", server.Namespace, namespace)
	}
	if got := server.Spec.NodeSelector[corev1.LabelHostname]; got != "node-0-hostname" {
		t.Fatalf("server hostname selector = %q, want node-0-hostname", got)
	}
	if got := server.Labels[podCIDRServerLabel]; got != ovnLifecycleTrafficServerRole {
		t.Fatalf("server label %q = %q, want %q", podCIDRServerLabel, got, ovnLifecycleTrafficServerRole)
	}

	targets := []string{"http://10.0.0.2:8080/", "http://ovn-lifecycle:8080/", "http://10.96.0.10:8080/"}
	probe := ovnLifecycleTrafficProbePod(namespace, "node-1-hostname", targets)
	if probe.Namespace != namespace {
		t.Fatalf("probe namespace = %q, want %q", probe.Namespace, namespace)
	}
	if got := probe.Spec.NodeSelector[corev1.LabelHostname]; got != "node-1-hostname" {
		t.Fatalf("probe hostname selector = %q, want node-1-hostname", got)
	}
	if !reflect.DeepEqual(probe.Spec.Containers[0].Args, targets) {
		t.Fatalf("probe targets = %v, want %v", probe.Spec.Containers[0].Args, targets)
	}
}
