// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/api"
)

const noVIPControlPlaneCount = 1

func validateNoVIPNodePoolScaleRefused(test *tenantTest) {
	It("refuses node-pool control-plane scale-up without kube-vip", func(ctx context.Context) {
		assertNoKubeVIP(ctx, test)

		pools := expectSucceed(test.nke.ListNodePools(ctx, test.cluster.ID))
		var controlPlanePool *api.NodePool
		for i := range pools {
			if pools[i].ControlPlane {
				controlPlanePool = &pools[i]
				break
			}
		}
		Expect(controlPlanePool).NotTo(BeNil(), "the no-VIP cluster should have one control-plane pool")
		Expect(controlPlanePool.DesiredNodeCount).To(Equal(noVIPControlPlaneCount))

		By("Attempting to increase the control-plane pool desired count")
		message := expectSucceed(test.nke.UpdateNodePoolDesiredCountExpectingRefusal(
			ctx,
			controlPlanePool.ID,
			noVIPControlPlaneCount+1,
		))
		Expect(message).To(ContainSubstring("controlPlaneVIP"))

		By("Verifying the rejected update did not change desired or actual capacity")
		pool, status, err := test.nke.GetNodePool(ctx, controlPlanePool.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(200))
		Expect(pool.DesiredNodeCount).To(Equal(noVIPControlPlaneCount))
		assertNoAdditionalControlPlaneNode(ctx, test)
	}, Label("lifecycle", "no-vip", "scale"), NodeTimeout(15*time.Minute))

	It("refuses control-plane node replacement without kube-vip", func(ctx context.Context) {
		assertNoKubeVIP(ctx, test)

		pools := expectSucceed(test.nke.ListNodePools(ctx, test.cluster.ID))
		var controlPlanePool *api.NodePool
		for i := range pools {
			if pools[i].ControlPlane {
				controlPlanePool = &pools[i]
				break
			}
		}
		Expect(controlPlanePool).NotTo(BeNil(), "the no-VIP cluster should have one control-plane pool")
		originalMemory := controlPlanePool.SystemReservedMemory
		Expect(originalMemory).NotTo(BeNil(), "the control-plane pool should report its system-reserved memory")
		originalMemoryValue := *originalMemory
		requestedMemory := "777Mi"
		Expect(originalMemoryValue).NotTo(Equal(requestedMemory), "rollout rejection requires a changed node-pool configuration")

		By("Attempting a control-plane node-pool update that would create a replacement node set")
		message := expectSucceed(test.nke.UpdateNodePoolSystemReservedMemoryExpectingRefusal(
			ctx,
			controlPlanePool.ID,
			requestedMemory,
		))
		Expect(message).To(ContainSubstring("controlPlaneVIP"))

		By("Verifying the rejected update did not change the pool or replace its node")
		pool, status, err := test.nke.GetNodePool(ctx, controlPlanePool.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(200))
		Expect(pool.SystemReservedMemory).To(HaveValue(Equal(originalMemoryValue)))
		assertNoAdditionalControlPlaneNode(ctx, test)
	}, Label("lifecycle", "no-vip", "rollout"), NodeTimeout(15*time.Minute))
}

func validateNoVIPControllerScaleRefused(test *tenantTest) {
	It("refuses controller-managed control-plane scale-up without kube-vip", func(ctx context.Context) {
		assertNoKubeVIP(ctx, test)

		By("Attempting to increase the cluster-level control-plane desired count")
		message := expectSucceed(test.nke.UpdateControlPlaneDesiredCountExpectingRefusal(
			ctx,
			test.cluster.ID,
			controlPlaneControllerUpdateDesiredCount,
		))
		Expect(message).To(ContainSubstring("controlPlaneVIP"))

		By("Verifying the rejected update did not change desired or actual capacity")
		controlPlane := expectSucceed(test.nke.GetClusterControlPlane(ctx, test.cluster.ID))
		Expect(controlPlane.Config.DesiredCount).To(Equal(noVIPControlPlaneCount))
		assertNoAdditionalControlPlaneNode(ctx, test)
	}, Label("lifecycle", "cp-controller", "no-vip", "scale"), NodeTimeout(15*time.Minute))
}

func assertNoKubeVIP(ctx context.Context, test *tenantTest) {
	GinkgoHelper()

	Expect(test.cluster.ControlPlaneVIP).To(BeNil(), "the cluster should report no control-plane VIP")
	tenantClient := expectSucceed(test.tenantClients.GetAdminClient(ctx, test.cluster.Name))
	var pods corev1.PodList
	Expect(tenantClient.List(
		ctx,
		&pods,
		client.InNamespace(kubeSystemNamespace),
		client.MatchingLabels{"component": "kube-vip"},
	)).To(Succeed())
	Expect(pods.Items).To(BeEmpty(), "a cluster without controlPlaneVIP should not run kube-vip")
}

func assertNoAdditionalControlPlaneNode(ctx context.Context, test *tenantTest) {
	GinkgoHelper()

	Consistently(func(ctx context.Context) error {
		nodes, err := test.nke.ListClusterNodesByControlPlane(ctx, test.cluster.ID, true)
		if err != nil {
			return err
		}
		if len(nodes) != noVIPControlPlaneCount {
			return fmt.Errorf("found %d control-plane nodes, want %d", len(nodes), noVIPControlPlaneCount)
		}
		return nil
	}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(5 * time.Second).Should(Succeed())
}
