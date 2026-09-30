// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func validateNoVIPUpgradeRefused(test *tenantTest) {
	It("refuses a control-plane upgrade without kube-vip", func(ctx context.Context) {
		assertNoKubeVIP(ctx, test)
		currentVersion := test.cluster.ControlPlaneKubernetesVersion
		targetVersion := upgradeKubernetesVersionChain[0]
		Expect(currentVersion).NotTo(Equal(targetVersion), "upgrade rejection requires a newer target version")

		By("Attempting a control-plane Kubernetes upgrade with maxSurge set to one")
		message := expectSucceed(test.nke.UpgradeClusterControlPlaneExpectingRefusal(
			ctx,
			test.cluster.ID,
			targetVersion,
		))
		Expect(message).To(ContainSubstring("controlPlaneVIP"))

		By("Verifying the rejected upgrade did not change version, surge, or node count")
		cluster := expectSucceed(test.nke.GetCluster(ctx, test.cluster.ID))
		Expect(cluster.ControlPlaneKubernetesVersion).To(Equal(currentVersion))
		Expect(cluster.ControlPlaneConfig).NotTo(BeNil())
		Expect(cluster.ControlPlaneConfig.MaxSurge).NotTo(BeNil())
		Expect(*cluster.ControlPlaneConfig.MaxSurge).To(Equal(0))
		assertNoAdditionalControlPlaneNode(ctx, test)
	}, Label("lifecycle", "no-vip", "upgrade"), NodeTimeout(15*time.Minute))
}
