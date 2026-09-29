// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// validateSingleNodeControlPlaneRotation exercises the manual single-node
// control-plane rotation path. On a cluster whose only
// control-plane node is A, it stages a replacement B (scale to two), then
// removes A.
//
// Removing A while B is the only other joined CP takes etcd from two voters to
// one. This verifies that expected transition completes without API overrides.
func validateSingleNodeControlPlaneRotation(test *tenantTest) {
	It("rotates a single-node control plane", func(ctx context.Context) {
		controlPlanePool := expectControlPlanePool(ctx, test)
		Expect(controlPlanePool.DesiredNodeCount).To(Equal(1),
			"single-node control-plane rotation requires a 1-CP cluster")

		originalNodes := expectNodeIDs(ctx, test, controlPlanePool.ID)
		Expect(originalNodes).To(HaveLen(1), "control-plane pool should have exactly one node before rotation")
		var original uuid.UUID
		for id := range originalNodes {
			original = id
		}

		By("Scaling the control-plane pool to two to stage a replacement")
		expectSucceed(test.nke.UpdateNodePoolDesiredCount(ctx, controlPlanePool.ID, 2))
		Expect(test.nke.WaitForProvisionedNodeCount(ctx, controlPlanePool.ID, 2)).To(Succeed())
		replacement := expectNewNode(ctx, test, controlPlanePool.ID, originalNodes)

		By("Restoring the desired count to one so healing does not re-add a node after removal")
		expectSucceed(test.nke.UpdateNodePoolDesiredCount(ctx, controlPlanePool.ID, 1))

		By("Removing the original control-plane node")
		Expect(test.nke.DeleteNode(ctx, original)).To(Succeed(),
			"the original control-plane node should be removed")

		By("Waiting for the control-plane pool to settle at the replacement node")
		Expect(test.nke.WaitForProvisionedNodeCountExcluding(ctx, controlPlanePool.ID, 1, original)).To(Succeed())

		By("Verifying the replacement is the surviving control-plane node")
		survivors := expectNodeIDs(ctx, test, controlPlanePool.ID)
		Expect(survivors).To(HaveKey(replacement.ID), "the staged replacement must be the surviving control-plane node")
		Expect(survivors).NotTo(HaveKey(original), "the rotated-out node must be gone")
	}, Label("lifecycle", "cp-rotation", "scale"), NodeTimeout(2*time.Hour))
}
