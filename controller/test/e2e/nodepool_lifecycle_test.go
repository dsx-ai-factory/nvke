// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/NVIDIA/nke/controller/test/e2e/internal/nke"
)

const (
	extraPoolName            = "worker-extra"
	cascadePoolName          = "worker-cascade"
	manualScaleDownStableFor = 5 * time.Minute
)

// validateClusterConfig verifies the cluster and its initial control-plane and
// worker node pools report the configuration they were created with (spec
// round-trip). NOTE: CWE's API Cluster type exposes no CNI-version field, so
// unlike the nke-e2e reference this cannot assert the CNI version.
func validateClusterConfig(test *tenantTest) {
	It("reports the cluster and initial node-pool configuration it was created with", func(ctx context.Context) {
		By("Verifying cluster-level configuration round-trips")
		Expect(test.cluster.CniPlugin).To(Equal(test.spec.CNIPlugin))
		Expect(test.cluster.ControlPlaneKubernetesVersion).To(Equal(test.spec.KubernetesVersion))
		Expect(test.cluster.EtcdEncrypted).To(Equal(test.spec.ControlPlane.EtcdEncrypted))

		By("Verifying the initial control-plane and worker node pools")
		cpPool, workerPool := expectScalePools(ctx, test)

		cpSpec := test.spec.ControlPlane.Pools[0]
		Expect(cpPool.ControlPlane).To(BeTrue())
		Expect(cpPool.ConfigProfile).To(Equal(cpSpec.ConfigProfile))
		Expect(cpPool.KubernetesVersion).To(Equal(test.spec.KubernetesVersion))
		Expect(cpPool.DesiredNodeCount).To(Equal(cpSpec.DesiredNodeCount))

		workerSpec := test.spec.Workers[0]
		Expect(workerPool.ControlPlane).To(BeFalse())
		Expect(workerPool.ConfigProfile).To(Equal(workerSpec.ConfigProfile))
		Expect(workerPool.KubernetesVersion).To(Equal(test.spec.KubernetesVersion))
		Expect(workerPool.DesiredNodeCount).To(Equal(workerSpec.DesiredNodeCount))
	}, Label("lifecycle", "nodepool", "config"), NodeTimeout(5*time.Minute))
}

// validateNodeReplacement verifies CWE's auto-heal contract: with the desired
// count unchanged, deleting a worker provisions a replacement while the
// surviving node is preserved. Runs at desired=2 (scales the worker pool up
// first) so "heal back with the survivor intact" is actually exercised, not
// just "recover to one node". Leaves the worker pool at desired=2.
func validateNodeReplacement(test *tenantTest) {
	It("replaces a worker node deleted while the desired count is unchanged, preserving the survivor", func(ctx context.Context) {
		_, workerPool := expectScalePools(ctx, test)

		By("Scaling the worker pool to two so replacement can be observed with a survivor")
		expectSucceed(test.nke.UpdateNodePoolDesiredCount(ctx, workerPool.ID, 2))
		Expect(test.nke.WaitForProvisionedNodeCount(ctx, workerPool.ID, 2)).To(Succeed())

		before := expectNodeIDs(ctx, test, workerPool.ID)
		Expect(before).To(HaveLen(2), "worker pool should have two provisioned nodes before replacement")
		var victim, survivor uuid.UUID
		for id := range before {
			if victim == uuid.Nil {
				victim = id
			} else {
				survivor = id
			}
		}

		By(fmt.Sprintf("Deleting worker node %s with the desired count unchanged at 2", victim))
		Expect(test.nke.DeleteNode(ctx, victim)).To(Succeed(), "should delete the worker node")

		By("Waiting for the pool to heal back to two provisioned nodes without the deleted node")
		Expect(test.nke.WaitForProvisionedNodeCountExcluding(ctx, workerPool.ID, 2, victim)).To(Succeed())

		By("Verifying the surviving node was preserved")
		Expect(expectNodeIDs(ctx, test, workerPool.ID)).To(HaveKey(survivor),
			"the surviving worker node must not be replaced")
	}, Label("lifecycle", "nodepool", "node-replacement", "scale"), NodeTimeout(2*time.Hour))
}

// validateManualScaleDownStability verifies two scale-down safety invariants
// that WaitForProvisionedNodeCount alone does not catch: (1) lowering the
// desired count does not by itself delete provisioned nodes, and (2) after a
// manual delete at the lowered count the pool stays put — no delayed
// re-provision. It scales the worker pool to two itself (so it does not depend
// on an earlier spec having left it there) and leaves it at one.
func validateManualScaleDownStability(test *tenantTest) {
	It("does not auto-remove or replace workers after scaling down and manually deleting one", func(ctx context.Context) {
		_, workerPool := expectScalePools(ctx, test)

		By("Scaling the worker pool to two to establish the precondition")
		expectSucceed(test.nke.UpdateNodePoolDesiredCount(ctx, workerPool.ID, 2))
		Expect(test.nke.WaitForProvisionedNodeCount(ctx, workerPool.ID, 2)).To(Succeed())

		By("Lowering the worker pool desired count from two to one")
		expectSucceed(test.nke.UpdateNodePoolDesiredCount(ctx, workerPool.ID, 1))

		By("Verifying scaling down alone did not remove any provisioned node")
		Expect(expectSucceed(test.nke.ProvisionedNodeCount(ctx, workerPool.ID))).To(Equal(2),
			"lowering the desired count must not auto-remove provisioned workers")

		nodes := expectNodeIDs(ctx, test, workerPool.ID)
		var victim uuid.UUID
		for id := range nodes {
			victim = id
			break
		}
		By(fmt.Sprintf("Manually deleting worker node %s", victim))
		Expect(test.nke.DeleteNode(ctx, victim)).To(Succeed())

		By("Waiting for the pool to reach one provisioned node")
		Expect(test.nke.WaitForProvisionedNodeCount(ctx, workerPool.ID, 1)).To(Succeed())

		By(fmt.Sprintf("Verifying the pool stays at one node (no delayed replacement) for %s", manualScaleDownStableFor))
		Expect(test.nke.WaitForProvisionedNodeCountStable(ctx, workerPool.ID, 1, manualScaleDownStableFor)).To(Succeed())
	}, Label("lifecycle", "nodepool", "scale", "manual-scale-down"), NodeTimeout(2*time.Hour))
}

// validateAdditionalNodePool creates a second worker node pool on the live
// cluster, scales it, confirms the primary worker pool is untouched, then
// deletes it and confirms it is gone.
func validateAdditionalNodePool(test *tenantTest) {
	It("creates, scales, and deletes an additional worker node pool", func(ctx context.Context) {
		_, primaryPool := expectScalePools(ctx, test)
		primaryBefore := expectNodeIDs(ctx, test, primaryPool.ID)

		By("Creating an additional worker node pool")
		spec := forgeNodePoolSpec(nke.NodePoolSpec{Name: extraPoolName, NodeNamePrefix: "wx", DesiredNodeCount: 1},
			extraPoolName, "wx", forgeOSProfile, forgeWConfigProfile)
		req := expectSucceed(nke.ForgeNodePoolRequest(test.cluster.ID, spec))
		extra := expectSucceed(test.nke.CreateNodePool(ctx, req))
		Expect(extra.Name).To(Equal(extraPoolName))
		Expect(extra.ControlPlane).To(BeFalse())

		By("Waiting for the additional pool to reach one provisioned node")
		Expect(test.nke.WaitForProvisionedNodeCount(ctx, extra.ID, 1)).To(Succeed())

		By("Scaling the additional pool from one node to two")
		expectSucceed(test.nke.UpdateNodePoolDesiredCount(ctx, extra.ID, 2))
		Expect(test.nke.WaitForProvisionedNodeCount(ctx, extra.ID, 2)).To(Succeed())

		By("Verifying the primary worker pool was not disturbed")
		Expect(expectNodeIDs(ctx, test, primaryPool.ID)).To(Equal(primaryBefore),
			"scaling the additional pool should not change the primary pool")

		By("Deleting the additional worker node pool")
		Expect(test.nke.DeleteNodePool(ctx, extra.ID)).To(Succeed())
		Expect(test.nke.WaitForNodePoolDeleted(ctx, extra.ID)).To(Succeed())
		Expect(expectSucceed(test.nke.ListNodePoolNodes(ctx, extra.ID))).To(BeEmpty(),
			"the deleted node pool should retain no nodes")
	}, Label("lifecycle", "nodepool", "scale"), NodeTimeout(2*time.Hour))
}

// validateNodePoolCascadePostCreate creates a worker node pool that is left
// active (never explicitly deleted) so the post-delete spec can prove cluster
// deletion cascades to it. It records the pool ID on the shared test state.
func validateNodePoolCascadePostCreate(test *tenantTest) {
	It("creates a worker node pool that stays active until cluster deletion", func(ctx context.Context) {
		spec := forgeNodePoolSpec(nke.NodePoolSpec{Name: cascadePoolName, NodeNamePrefix: "wc", DesiredNodeCount: 1},
			cascadePoolName, "wc", forgeOSProfile, forgeWConfigProfile)
		req := expectSucceed(nke.ForgeNodePoolRequest(test.cluster.ID, spec))
		pool := expectSucceed(test.nke.CreateNodePool(ctx, req))
		Expect(test.nke.WaitForProvisionedNodeCount(ctx, pool.ID, 1)).To(Succeed())
		test.cascadePoolID = pool.ID
	}, Label("lifecycle", "nodepool", "cascade"), NodeTimeout(2*time.Hour))
}

// validateNodePoolCascadePostDelete proves that deleting the cluster removed the
// still-active worker node pool created in validateNodePoolCascadePostCreate.
func validateNodePoolCascadePostDelete(test *tenantTest) {
	It("removes the still-active worker node pool when the cluster is deleted", func(ctx context.Context) {
		Expect(test.cascadePoolID).NotTo(Equal(uuid.Nil), "cascade pool should have been created post-create")

		By("Verifying the node pool no longer exists")
		pool, status, err := test.nke.GetNodePool(ctx, test.cascadePoolID)
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(404), "cluster deletion should remove the active node pool")
		Expect(pool).To(BeNil())

		By("Verifying the node pool retains no nodes")
		Expect(expectSucceed(test.nke.ListNodePoolNodes(ctx, test.cascadePoolID))).To(BeEmpty())
	}, Label("cascade"), NodeTimeout(10*time.Minute))
}
