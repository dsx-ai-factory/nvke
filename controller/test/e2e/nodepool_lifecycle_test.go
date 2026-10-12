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

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
)

const (
	extraPoolName            = "worker-extra"
	cascadePoolName          = "worker-cascade"
	manualScaleDownStableFor = 5 * time.Minute
)

// validateClusterConfig verifies the cluster and its initial control-plane and
// worker node pools report the configuration they were created with (spec
// round-trip). NOTE: the controller's API Cluster type exposes no CNI-version field, so
// unlike the nke-e2e reference this cannot assert the CNI version.
func validateClusterConfig(test *tenantTest) {
	It("reports the cluster and initial node-pool configuration it was created with", func(ctx context.Context) {
		By("Verifying cluster-level configuration round-trips")
		Expect(test.cluster.CniPlugin).To(Equal(test.spec.CNIPlugin))
		Expect(test.cluster.ControlPlaneKubernetesVersion).To(Equal(test.spec.KubernetesVersion))
		Expect(test.cluster.EtcdEncrypted).To(Equal(test.spec.ControlPlane.EtcdEncrypted))

		By("Verifying the cluster-level control-plane configuration and that no control-plane node pool exists")
		cpSpec := test.spec.ControlPlane.Config
		Expect(cpSpec).NotTo(BeNil(), "the cluster must be created with a control-plane config")
		controlPlane := expectSucceed(test.nvke.GetClusterControlPlane(ctx, test.cluster.ID))
		if controlPlane.Config.Nico != nil {
			Expect(controlPlane.Config.Nico.ConfigProfile).To(Equal(cpSpec.ConfigProfile))
		}
		Expect(controlPlane.Status.KubernetesVersion).To(Equal(test.spec.KubernetesVersion))
		Expect(controlPlane.Config.DesiredCount).To(Equal(cpSpec.DesiredCount))
		for _, pool := range expectSucceed(test.nvke.ListNodePools(ctx, test.cluster.ID)) {
			Expect(pool.ControlPlane).To(BeFalse(), "a controller-managed control plane has no node pool (found %q)", pool.Name)
		}

		By("Verifying the initial worker node pool")
		workerPool := expectWorkerPool(ctx, test)
		workerSpec := test.spec.Workers[0]
		Expect(workerPool.ControlPlane).To(BeFalse())
		if workerPool.Nico != nil {
			Expect(workerPool.Nico.ConfigProfile).To(Equal(workerSpec.ConfigProfile))
		}
		Expect(workerPool.KubernetesVersion).To(Equal(test.spec.KubernetesVersion))
		Expect(workerPool.DesiredNodeCount).To(Equal(workerSpec.DesiredNodeCount))
	}, Label("lifecycle", "nodepool", "config"), NodeTimeout(5*time.Minute))
}

// validateNodeReplacement verifies the controller's auto-heal contract: with the desired
// count unchanged, deleting a worker provisions a replacement while the
// surviving node is preserved. Runs at desired=2 (scales the worker pool up
// first) so "heal back with the survivor intact" is actually exercised, not
// just "recover to one node". Leaves the worker pool at desired=2.
func validateNodeReplacement(test *tenantTest) {
	It("replaces a worker node deleted while the desired count is unchanged, preserving the survivor", func(ctx context.Context) {
		workerPool := expectWorkerPool(ctx, test)

		By("Scaling the worker pool to two so replacement can be observed with a survivor")
		expectSucceed(test.nvke.UpdateNodePoolDesiredCount(ctx, workerPool.ID, 2))
		Expect(test.nvke.WaitForProvisionedNodeCount(ctx, workerPool.ID, 2)).To(Succeed())

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
		Expect(test.nvke.DeleteNode(ctx, victim)).To(Succeed(), "should delete the worker node")

		By("Waiting for the pool to heal back to two provisioned nodes without the deleted node")
		Expect(test.nvke.WaitForProvisionedNodeCountExcluding(ctx, workerPool.ID, 2, victim)).To(Succeed())

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
		workerPool := expectWorkerPool(ctx, test)

		By("Scaling the worker pool to two to establish the precondition")
		expectSucceed(test.nvke.UpdateNodePoolDesiredCount(ctx, workerPool.ID, 2))
		Expect(test.nvke.WaitForProvisionedNodeCount(ctx, workerPool.ID, 2)).To(Succeed())

		By("Lowering the worker pool desired count from two to one")
		expectSucceed(test.nvke.UpdateNodePoolDesiredCount(ctx, workerPool.ID, 1))

		By("Verifying scaling down alone did not remove any provisioned node")
		Expect(expectSucceed(test.nvke.ProvisionedNodeCount(ctx, workerPool.ID))).To(Equal(2),
			"lowering the desired count must not auto-remove provisioned workers")

		nodes := expectNodeIDs(ctx, test, workerPool.ID)
		var victim uuid.UUID
		for id := range nodes {
			victim = id
			break
		}
		By(fmt.Sprintf("Manually deleting worker node %s", victim))
		Expect(test.nvke.DeleteNode(ctx, victim)).To(Succeed())

		By("Waiting for the pool to reach one provisioned node")
		Expect(test.nvke.WaitForProvisionedNodeCount(ctx, workerPool.ID, 1)).To(Succeed())

		By(fmt.Sprintf("Verifying the pool stays at one node (no delayed replacement) for %s", manualScaleDownStableFor))
		Expect(test.nvke.WaitForProvisionedNodeCountStable(ctx, workerPool.ID, 1, manualScaleDownStableFor)).To(Succeed())
	}, Label("lifecycle", "nodepool", "scale", "manual-scale-down"), NodeTimeout(2*time.Hour))
}

// validateAdditionalNodePool creates a second worker node pool on the live
// cluster, scales it, confirms the primary worker pool is untouched, then
// deletes it and confirms it is gone.
func validateAdditionalNodePool(test *tenantTest) {
	It("creates, scales, and deletes an additional worker node pool", func(ctx context.Context) {
		primaryPool := expectWorkerPool(ctx, test)
		primaryBefore := expectNodeIDs(ctx, test, primaryPool.ID)

		By("Creating an additional worker node pool")
		req := expectSucceed(workerNodePoolRequest(test, extraPoolName, "wx"))
		extra := expectSucceed(test.nvke.CreateNodePool(ctx, req))
		Expect(extra.Name).To(Equal(extraPoolName))
		Expect(extra.ControlPlane).To(BeFalse())

		By("Waiting for the additional pool to reach one provisioned node")
		Expect(test.nvke.WaitForProvisionedNodeCount(ctx, extra.ID, 1)).To(Succeed())

		By("Scaling the additional pool from one node to two")
		expectSucceed(test.nvke.UpdateNodePoolDesiredCount(ctx, extra.ID, 2))
		Expect(test.nvke.WaitForProvisionedNodeCount(ctx, extra.ID, 2)).To(Succeed())

		By("Verifying the primary worker pool was not disturbed")
		Expect(expectNodeIDs(ctx, test, primaryPool.ID)).To(Equal(primaryBefore),
			"scaling the additional pool should not change the primary pool")

		By("Deleting the additional worker node pool")
		Expect(test.nvke.DeleteNodePool(ctx, extra.ID)).To(Succeed())
		Expect(test.nvke.WaitForNodePoolDeleted(ctx, extra.ID)).To(Succeed())
		Expect(expectSucceed(test.nvke.ListNodePoolNodes(ctx, extra.ID))).To(BeEmpty(),
			"the deleted node pool should retain no nodes")
	}, Label("lifecycle", "nodepool", "scale"), NodeTimeout(2*time.Hour))
}

// validateNodePoolCascadePostCreate creates a worker node pool that is left
// active (never explicitly deleted) so the post-delete spec can prove cluster
// deletion cascades to it. It records the pool ID on the shared test state.
func validateNodePoolCascadePostCreate(test *tenantTest) {
	It("creates a worker node pool that stays active until cluster deletion", func(ctx context.Context) {
		req := expectSucceed(workerNodePoolRequest(test, cascadePoolName, "wc"))
		pool := expectSucceed(test.nvke.CreateNodePool(ctx, req))
		Expect(test.nvke.WaitForProvisionedNodeCount(ctx, pool.ID, 1)).To(Succeed())
		test.cascadePoolID = pool.ID
	}, Label("lifecycle", "nodepool", "cascade"), NodeTimeout(2*time.Hour))
}

// validateNodePoolCascadePostDelete proves that deleting the cluster removed the
// still-active worker node pool created in validateNodePoolCascadePostCreate.
func validateNodePoolCascadePostDelete(test *tenantTest) {
	It("removes the still-active worker node pool when the cluster is deleted", func(ctx context.Context) {
		Expect(test.cascadePoolID).NotTo(Equal(uuid.Nil), "cascade pool should have been created post-create")

		By("Verifying the node pool no longer exists")
		pool, status, err := test.nvke.GetNodePool(ctx, test.cascadePoolID)
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(404), "cluster deletion should remove the active node pool")
		Expect(pool).To(BeNil())

		By("Verifying the node pool retains no nodes")
		Expect(expectSucceed(test.nvke.ListNodePoolNodes(ctx, test.cascadePoolID))).To(BeEmpty())
	}, Label("cascade"), NodeTimeout(10*time.Minute))
}

func workerNodePoolRequest(test *tenantTest, name, nodeNamePrefix string) (api.CreateNodePoolRequest, error) {
	base := nvke.NodePoolSpec{Name: name, NodeNamePrefix: nodeNamePrefix, DesiredNodeCount: 1}
	if test.spec.Backend == nvke.BackendCAPD {
		spec := capdNodePoolSpec(base, name, nodeNamePrefix)
		return nvke.CAPDNodePoolRequest(test.cluster.ID, spec, test.spec.CAPDNodeImage)
	}
	if test.spec.Backend == nvke.BackendNicoVMS {
		base.InstanceTypeID = nicoVMSLargeInstanceID
		spec := nicoVMSNodePoolSpec(base, name, nodeNamePrefix, nicoVMSOSProfile, nicoVMSWConfigProfile)
		return nvke.NicoVMSNodePoolRequest(test.cluster.ID, spec)
	}
	if test.spec.Backend == nvke.BackendNicoContainers {
		// nicoNodePoolSpecs is what the cluster-create path uses, so a pool
		// added later is shaped the same as the ones created with the cluster.
		spec := nicoNodePoolSpecs([]nvke.NodePoolSpec{base}, name, nodeNamePrefix)[0]
		return nvke.NicoNodePoolRequest(test.cluster.ID, test.cluster.Name, nicoVpcID, spec)
	}
	return api.CreateNodePoolRequest{}, fmt.Errorf("node pool lifecycle is unsupported for backend %q", test.spec.Backend)
}
