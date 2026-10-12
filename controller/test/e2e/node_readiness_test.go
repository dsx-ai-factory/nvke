// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
)

// validateNodeReadinessPostCreate checks that the complete desired topology is
// Ready through both user-facing surfaces: the controller's API and the tenant Kubernetes
// API itself.
func validateNodeReadinessPostCreate(test *tenantTest) {
	It("brings every desired node to Ready", func(ctx context.Context) {
		expectedPools := expectedNodeCounts(test.spec)
		expectedNodeCount := clusterNodeCount(test.spec)

		// On the way out rather than at each failed poll. The assertion below
		// returns the first node that is not ready and says nothing about the
		// rest, and a spec timeout reports no node at all, so the one failure
		// this lane keeps hitting is the one that collected nothing: the
		// provision succeeds, so the collector on that path never fires.
		//
		// Detached because a spec that timed out arrives here with its
		// context already canceled, which is exactly when this matters.
		detached := context.WithoutCancel(ctx)
		DeferCleanup(func() {
			if !CurrentSpecReport().Failed() || test.cluster == nil {
				return
			}
			test.collectDiagnostics(detached, test.cluster.ID)
		})

		By("Asserting the controller reports KubernetesReady for every desired node")
		Eventually(test.nodesReadyWithProgress(readinessProgressEvery)).WithContext(ctx).
			WithTimeout(test.timeouts.Provision).WithPolling(1*time.Second).
			Should(Succeed(), "all desired nodes should become ready in the controller")

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
		// 45 minutes is a canary, not a budget. This spec took 844s, just
		// over 14 minutes, in run 36691646226, the passing arm64 GB200 run,
		// so a run that exceeds the cap is a 3x regression and the cap is the
		// signal. Raising it to match Provision's 90 minutes buys longer
		// failures and no information: the runs that hit this died at exactly
		// 2700s, and when the cap was moved out of the way the run burned the
		// full 90 minutes earlier in the chain instead.
	}, Label("readiness"), SpecTimeout(45*time.Minute))
}

// expectedNodeCounts returns the desired node count of every node pool in spec,
// keyed by pool name. The control plane has no pool, so its nodes are not in the
// map; see controlPlaneConfigNodeCount.
func expectedNodeCounts(spec nvke.ClusterSpec) map[string]int {
	counts := make(map[string]int, len(spec.Workers))
	for _, pool := range spec.Workers {
		counts[pool.Name] = pool.DesiredNodeCount
	}
	return counts
}

// controlPlaneConfigNodeCount returns how many nodes spec's cluster-level
// controlPlaneConfig asks for: the nodes that belong to no node pool.
func controlPlaneConfigNodeCount(spec nvke.ClusterSpec) int {
	if spec.ControlPlane.Config == nil {
		return 0
	}
	return spec.ControlPlane.Config.DesiredCount
}

// controlPlaneNodeCount returns how many control-plane nodes spec asks for.
func controlPlaneNodeCount(spec nvke.ClusterSpec) int {
	return controlPlaneConfigNodeCount(spec)
}

// clusterNodeCount returns how many nodes spec asks for in total: every
// node-pool node plus the nodes of a controlPlaneConfig.
func clusterNodeCount(spec nvke.ClusterSpec) int {
	return totalExpectedNodes(expectedNodeCounts(spec)) + controlPlaneConfigNodeCount(spec)
}

func totalExpectedNodes(expectedPools map[string]int) int {
	total := 0
	for _, count := range expectedPools {
		total += count
	}
	return total
}

// readinessProgressEvery is how often the wait below says what it is waiting
// for. The assertion polls every second, so logging each failure would write
// 2700 identical lines over a full budget; a minute is often enough to show a
// node changing state and rare enough to read.
const readinessProgressEvery = time.Minute

// nodesReadyWithProgress is assertDesiredNodesReady, reporting what is still
// outstanding as it goes.
//
// Without it the readiness wait is silent for its whole budget, so a run that
// is making progress and one that is stuck look identical until the timeout,
// and the timeout names only whichever node happened to fail last.
func (t *tenantTest) nodesReadyWithProgress(every time.Duration) func(context.Context) error {
	start := time.Now()
	var last time.Time
	return func(ctx context.Context) error {
		err := t.assertDesiredNodesReady(ctx)
		if err != nil && time.Since(last) >= every {
			last = time.Now()
			t.log.Info("waiting for nodes to become ready",
				"elapsed", time.Since(start).Round(time.Second).String(),
				"outstanding", err.Error())
		}
		return err
	}
}

// assertDesiredNodesReady checks that the controller reports every desired node of the
// cluster KubernetesReady, including a controller-managed control plane. The
// caller controls retries.
func (t *tenantTest) assertDesiredNodesReady(ctx context.Context) error {
	if err := t.nvke.AssertNodeConditions(ctx, t.cluster.ID, expectedNodeCounts(t.spec), nvke.IsNodeReady); err != nil {
		return err
	}
	if desired := controlPlaneConfigNodeCount(t.spec); desired > 0 {
		return t.nvke.AssertControlPlaneNodesReady(ctx, t.cluster.ID, desired)
	}
	return nil
}

// validateNodesReadyInCluster checks that the nodes of expectedPools, plus the
// desired controller-managed control-plane nodes, are Ready in the tenant
// cluster. It returns their names.
func validateNodesReadyInCluster(
	ctx context.Context,
	test *tenantTest,
	tenantClient client.Client,
	expectedPools map[string]int,
) ([]string, error) {
	expectedNodes, err := test.nvke.ListExpectedNodes(ctx, test.cluster.ID, expectedPools)
	if err != nil {
		return nil, err
	}

	expectedNodeNames := make([]string, 0, len(expectedNodes))
	for _, node := range expectedNodes {
		expectedNodeNames = append(expectedNodeNames, node.Name)
	}
	if test.spec.ControlPlane.Config != nil {
		desired, err := currentControllerControlPlaneCount(ctx, test)
		if err != nil {
			return nil, err
		}
		controlPlaneNames, err := test.nvke.ControlPlaneNodeNames(ctx, test.cluster.ID)
		if err != nil {
			return nil, err
		}
		if len(controlPlaneNames) != desired {
			return nil, fmt.Errorf("controller reports %d controller-managed control-plane nodes; want %d", len(controlPlaneNames), desired)
		}
		expectedNodeNames = append(expectedNodeNames, controlPlaneNames...)
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
