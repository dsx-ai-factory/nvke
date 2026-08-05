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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/NVIDIA/nke/controller/api"
)

const (
	upgradeStartingKubernetesVersion = "1.34.2"
	upgradeTargetKubernetesVersion   = "1.35.3"
)

func validateK8sUpgrade(test *tenantTest) {
	It("upgrades the control plane Kubernetes version", func(ctx context.Context) {
		targetVersion := upgradeTargetKubernetesVersion
		Expect(test.spec.KubernetesVersion).NotTo(Equal(targetVersion), "upgrade validation requires the cluster to be created below the target version")

		controlPlanePool, err := expectControlPlanePool(ctx, test)
		Expect(err).NotTo(HaveOccurred(), "should find the control-plane pool")

		originalDesiredCount := controlPlanePool.DesiredNodeCount
		originalControlPlaneNodes := expectNodeIDs(ctx, test, controlPlanePool.ID)

		By(fmt.Sprintf("Upgrading the control plane from %s to %s", test.spec.KubernetesVersion, targetVersion))
		upgraded, err := test.nke.UpgradeClusterControlPlane(ctx, test.cluster.ID, targetVersion)
		Expect(err).NotTo(HaveOccurred(), "should request the control-plane upgrade")
		Expect(upgraded.ControlPlaneKubernetesVersion).To(Equal(targetVersion))

		By("Observing the control-plane upgrade start")
		Eventually(func(ctx context.Context) bool {
			cluster, err := test.nke.GetCluster(ctx, test.cluster.ID)
			if err != nil {
				return false
			}
			return conditionIsTrue(cluster.Conditions.ControlPlaneUpgrading) || conditionIsTrue(cluster.Conditions.CpNodesRecycling)
		}).WithContext(ctx).WithTimeout(30*time.Minute).WithPolling(test.timeouts.Poll).
			Should(BeTrue(), "control-plane upgrade should enter an in-progress state")

		By("Waiting for the control-plane upgrade to complete")
		cluster, err := test.nke.WaitForControlPlaneUpgradeComplete(ctx, test.cluster.ID, targetVersion)
		Expect(err).NotTo(HaveOccurred(), "control-plane upgrade should complete")
		test.cluster = cluster

		By("Verifying recycled control-plane nodes were replaced")
		currentControlPlaneNodes, err := test.nke.ListNodePoolNodes(ctx, controlPlanePool.ID)
		Expect(err).NotTo(HaveOccurred(), "should list control-plane nodes after upgrade")
		Expect(currentControlPlaneNodes).To(HaveLen(originalDesiredCount), "control-plane pool should retain its desired node count")

		for _, node := range currentControlPlaneNodes {
			if _, existed := originalControlPlaneNodes[node.ID]; existed {
				Fail(fmt.Sprintf("control-plane node %s should have been recycled during upgrade", node.ID))
			}
			Expect(node.Conditions.Provisioned.Status).To(Equal(api.ConditionStatusTrue), "replacement control-plane node %s should be provisioned", node.Name)
			Expect(node.Conditions.KubernetesReady.Status).To(Equal(api.ConditionStatusTrue), "replacement control-plane node %s should be KubernetesReady", node.Name)
		}
	}, Label("lifecycle", "upgrade"), NodeTimeout(3*time.Hour))
}

func expectControlPlanePool(ctx context.Context, test *tenantTest) (api.NodePool, error) {
	GinkgoHelper()

	pools, err := test.nke.ListNodePools(ctx, test.cluster.ID)
	if err != nil {
		return api.NodePool{}, err
	}
	for i := range pools {
		if pools[i].ControlPlane {
			return pools[i], nil
		}
	}
	return api.NodePool{}, fmt.Errorf("cluster %s has no control-plane pool", test.cluster.ID)
}

func conditionIsTrue(condition *api.Condition) bool {
	return condition != nil && condition.Status == api.ConditionStatusTrue
}
