// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package e2e

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/test/e2e/internal/nke"
)

// validateNodeReadinessPostCreate checks that the complete desired topology is
// Ready through both user-facing surfaces: CWE's API and the tenant Kubernetes
// API itself.
func validateNodeReadinessPostCreate(test *tenantTest) {
	It("brings every desired node to Ready", func(ctx context.Context) {
		expectedPools := expectedNodeCounts(test.spec)
		expectedNodeCount := totalExpectedNodes(expectedPools)

		By("Asserting CWE reports KubernetesReady for every desired node")
		Eventually(func(ctx context.Context) error {
			return test.nke.AssertNodeConditions(ctx, test.cluster.ID, expectedPools, nke.IsNodeReady)
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(1*time.Second).
			Should(Succeed(), "all desired nodes should become ready in CWE")

		By("Asserting the tenant cluster reports Ready for every desired node")
		test.log.Info("Building an authenticated client for the tenant cluster")
		var tenantClient client.Client
		Eventually(func(ctx context.Context) error {
			var err error
			tenantClient, err = test.tenantClients.GetClient(ctx, test.cluster.Name)
			return err
		}).WithContext(ctx).WithTimeout(1 * time.Minute).WithPolling(1 * time.Second).Should(Succeed())
		var readyNodeNames []string
		test.log.Info("Checking nodes in tenant cluster")
		Eventually(func(ctx context.Context) error {
			var err error
			readyNodeNames, err = validateNodesReadyInCluster(ctx, test, tenantClient, expectedPools)
			return err
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(1 * time.Second).Should(Succeed())

		test.log.Info("All desired tenant nodes are ready", "count", expectedNodeCount, "nodes", readyNodeNames)
	}, Label("readiness"), SpecTimeout(45*time.Minute))
}

func expectedNodeCounts(spec nke.ClusterSpec) map[string]int {
	counts := make(map[string]int, len(spec.ControlPlane.Pools)+len(spec.Workers))
	for _, pool := range spec.ControlPlane.Pools {
		counts[pool.Name] = pool.DesiredNodeCount
	}
	for _, pool := range spec.Workers {
		counts[pool.Name] = pool.DesiredNodeCount
	}
	return counts
}

func totalExpectedNodes(expectedPools map[string]int) int {
	total := 0
	for _, count := range expectedPools {
		total += count
	}
	return total
}

// validateNodesReadyInCluster checks that all the expected nodes are reported Ready in
// the tenant cluster.
func validateNodesReadyInCluster(
	ctx context.Context,
	test *tenantTest,
	tenantClient client.Client,
	expectedPools map[string]int,
) ([]string, error) {
	expectedNodes, err := test.nke.ListExpectedNodes(ctx, test.cluster.ID, expectedPools)
	if err != nil {
		return nil, err
	}

	expectedNodeNames := make([]string, 0, len(expectedNodes))
	for _, node := range expectedNodes {
		expectedNodeNames = append(expectedNodeNames, node.Name)
	}
	sort.Strings(expectedNodeNames)

	var tenantNodes corev1.NodeList
	if err := tenantClient.List(ctx, &tenantNodes); err != nil {
		return nil, fmt.Errorf("listing tenant Kubernetes nodes: %w", err)
	}
	for _, nodeName := range expectedNodeNames {
		nodeIndex := slices.IndexFunc(tenantNodes.Items, func(node corev1.Node) bool {
			return node.Name == nodeName
		})
		if nodeIndex < 0 {
			return nil, fmt.Errorf("tenant cluster does not report expected node %q", nodeName)
		}
		ready, detail := kubernetesNodeReady(&tenantNodes.Items[nodeIndex])
		if !ready {
			return nil, fmt.Errorf("tenant Kubernetes node %q is not Ready: %s", nodeName, detail)
		}
	}

	return expectedNodeNames, nil
}

func kubernetesNodeReady(node *corev1.Node) (bool, string) {
	for _, condition := range node.Status.Conditions {
		if condition.Type != corev1.NodeReady {
			continue
		}
		if condition.Status == corev1.ConditionTrue {
			return true, string(condition.Status)
		}
		return false, fmt.Sprintf("%s (%s: %s)", condition.Status, condition.Reason, condition.Message)
	}
	return false, "Ready condition missing"
}
