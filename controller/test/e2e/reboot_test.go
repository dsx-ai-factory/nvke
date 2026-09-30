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
)

const (
	rebootLabelKey    = "nke.nvidia.com/reboot"
	rebootLabelReason = "e2e-label-reboot"
)

func validateLabelReboot(test *tenantTest) {
	It("reboots a worker node via the nke.nvidia.com/reboot label", func(ctx context.Context) {
		workerPool := expectWorkerPool(ctx, test)

		By("Waiting for a Ready worker to reboot")
		worker := expectSucceed(test.nke.WaitForReadyNodeInPool(ctx, workerPool.ID))
		rebootObservedAfter := worker.Conditions.Rebooting.LastTransitionTime

		By(fmt.Sprintf("Applying %s=%s to worker %s", rebootLabelKey, rebootLabelReason, worker.Name))
		applyRebootLabel(ctx, test, worker.Name)

		By("Waiting for CWE to observe the reboot")
		expectSucceed(test.nke.WaitForNodeRebooting(ctx, worker.ID, rebootObservedAfter))

		By("Waiting for reboot to complete (Rebooting=False and KubernetesReady=True)")
		expectSucceed(test.nke.WaitForNodeRebootComplete(ctx, worker.ID))
		validateOVNLifecycleTraffic(ctx, test, "worker reboot", worker.Name)
	}, Label("lifecycle", "reboot"), NodeTimeout(45*time.Minute))
}

func applyRebootLabel(ctx context.Context, test *tenantTest, nodeName string) {
	GinkgoHelper()

	tenantClient := expectSucceed(test.tenantClients.GetClient(ctx, test.cluster.Name))

	var node corev1.Node
	Expect(tenantClient.Get(ctx, client.ObjectKey{Name: nodeName}, &node)).To(Succeed(), "should get tenant node")
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	node.Labels[rebootLabelKey] = rebootLabelReason
	Expect(tenantClient.Update(ctx, &node)).To(Succeed(), "should label tenant node for reboot")
}
