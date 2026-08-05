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

	openapi_types "github.com/oapi-codegen/runtime/types"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/sync/errgroup"

	"github.com/NVIDIA/nke/controller/api"
)

func validateNodePoolScale(test *tenantTest) {
	It("scales a control-plane and worker node up and down", func(ctx context.Context) {
		controlPlanePool, workerPool := expectScalePools(ctx, test)
		originalControlPlaneCount := controlPlanePool.DesiredNodeCount
		originalWorkerCount := workerPool.DesiredNodeCount
		originalControlPlaneNodes := expectNodeIDs(ctx, test, controlPlanePool.ID)
		originalWorkerNodes := expectNodeIDs(ctx, test, workerPool.ID)

		By("Incrementing the desired count of the control-plane and worker pools by 1")
		expectSucceed(test.nke.UpdateNodePoolDesiredCount(ctx, controlPlanePool.ID, originalControlPlaneCount+1))
		expectSucceed(test.nke.UpdateNodePoolDesiredCount(ctx, workerPool.ID, originalWorkerCount+1))

		By("Waiting for the control-plane and worker pools to reach the new desired count")
		waitForPoolCountsInParallel(ctx, test,
			poolScale{pool: controlPlanePool, desired: originalControlPlaneCount + 1},
			poolScale{pool: workerPool, desired: originalWorkerCount + 1},
		)
		controlPlaneNode := expectNewNode(ctx, test, controlPlanePool.ID, originalControlPlaneNodes)
		workerNode := expectNewNode(ctx, test, workerPool.ID, originalWorkerNodes)

		By("Decrementing the desired count of the control-plane and worker pools by 1")
		expectSucceed(test.nke.UpdateNodePoolDesiredCount(ctx, controlPlanePool.ID, originalControlPlaneCount))
		expectSucceed(test.nke.UpdateNodePoolDesiredCount(ctx, workerPool.ID, originalWorkerCount))

		// Scaling the desired count down by itself does not delete any nodes.
		By("Deleting the scaled-up control-plane and worker nodes")
		Expect(test.nke.DeleteNode(ctx, controlPlaneNode.ID)).To(Succeed(), "should delete the scaled-up control-plane node")
		Expect(test.nke.DeleteNode(ctx, workerNode.ID)).To(Succeed(), "should delete the scaled-up worker node")

		By("Waiting for the control-plane and worker pools to reach the new desired count")
		waitForPoolCountsInParallel(ctx, test,
			poolScale{pool: controlPlanePool, desired: originalControlPlaneCount},
			poolScale{pool: workerPool, desired: originalWorkerCount},
		)
	}, Label("lifecycle", "scale"), NodeTimeout(2*time.Hour))
}

type poolScale struct {
	pool    api.NodePool
	desired int
}

func expectScalePools(ctx context.Context, test *tenantTest) (api.NodePool, api.NodePool) {
	GinkgoHelper()

	pools, err := test.nke.ListNodePools(ctx, test.cluster.ID)
	Expect(err).NotTo(HaveOccurred(), "should list node pools")

	var controlPlanePool *api.NodePool
	var workerPool *api.NodePool
	for i := range pools {
		if pools[i].ControlPlane {
			controlPlanePool = &pools[i]
			continue
		}
		workerPool = &pools[i]
	}

	Expect(controlPlanePool).NotTo(BeNil(), "cluster should have a control-plane pool")
	Expect(workerPool).NotTo(BeNil(), "cluster should have a worker pool")

	return *controlPlanePool, *workerPool
}

func expectNodeIDs(ctx context.Context, test *tenantTest, nodePoolID openapi_types.UUID) map[openapi_types.UUID]struct{} {
	GinkgoHelper()

	nodes, err := test.nke.ListNodePoolNodes(ctx, nodePoolID)
	Expect(err).NotTo(HaveOccurred(), "should list nodes")

	nodeIDs := make(map[openapi_types.UUID]struct{}, len(nodes))
	for _, node := range nodes {
		nodeIDs[node.ID] = struct{}{}
	}
	return nodeIDs
}

func expectNewNode(ctx context.Context, test *tenantTest, nodePoolID openapi_types.UUID, originalNodes map[openapi_types.UUID]struct{}) api.Node {
	GinkgoHelper()

	nodes, err := test.nke.ListNodePoolNodes(ctx, nodePoolID)
	Expect(err).NotTo(HaveOccurred(), "should list nodes")

	for _, node := range nodes {
		if _, existed := originalNodes[node.ID]; !existed {
			return node
		}
	}
	Fail(fmt.Sprintf("expected a new node in node pool %s", nodePoolID))
	return api.Node{}
}

func waitForPoolCountsInParallel(ctx context.Context, test *tenantTest, scales ...poolScale) {
	GinkgoHelper()

	group, groupCtx := errgroup.WithContext(ctx)
	for _, scale := range scales {
		scale := scale
		group.Go(func() error {
			if err := test.nke.WaitForProvisionedNodeCount(groupCtx, scale.pool.ID, scale.desired); err != nil {
				return fmt.Errorf("waiting for node pool %s to have %d nodes: %w", scale.pool.Name, scale.desired, err)
			}
			return nil
		})
	}
	Expect(group.Wait()).To(Succeed())
}
