// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"os"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/NVIDIA/nke/controller/test/e2e/internal/nke"
)

// validateSingleNodeControlPlaneRotation exercises the primary NKX-13160 use
// case: manual single-node control-plane rotation. On a cluster whose only
// control-plane node is A, it stages a replacement B (scale to two), then
// force-removes A with the etcd single-voter override.
//
// Why the override is required: removing A while B is the only other joined CP
// takes etcd from two voters to one. CWE's etcd-member-remove dry-run reports
// the single-voter condition (exit 3) and blocks the teardown unless
// AllowSingleVoter is set. (Deleting the *last* CP would instead skip live
// removal entirely per NKX-13735, so it does not exercise this path.)
//
// This asserts only the positive path — that the override lets the rotation
// complete. The negative (blocked without the flag) is covered deterministically
// by the CWE unit/workflow tests, and asserting an async teardown failure here
// would be flaky. Requires CWE deployed with ALLOW_ETCD_SAFETY_OVERRIDES=true;
// self-skips unless E2E_ETCD_OVERRIDES_ENABLED=true.
func validateSingleNodeControlPlaneRotation(test *tenantTest) {
	It("rotates a single-node control plane using the etcd single-voter override", func(ctx context.Context) {
		if os.Getenv("E2E_ETCD_OVERRIDES_ENABLED") != "true" {
			Skip("requires CWE deployed with ALLOW_ETCD_SAFETY_OVERRIDES=true; set E2E_ETCD_OVERRIDES_ENABLED=true to run")
		}

		controlPlanePool, _ := expectScalePools(ctx, test)
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

		By("Force-removing the original control-plane node with the single-voter override")
		Expect(test.nke.DeleteNode(ctx, original, nke.WithAllowSingleVoter())).To(Succeed(),
			"single-voter override should let the original control-plane node be removed")

		By("Waiting for the control-plane pool to settle at the replacement node")
		Expect(test.nke.WaitForProvisionedNodeCountExcluding(ctx, controlPlanePool.ID, 1, original)).To(Succeed())

		By("Verifying the replacement is the surviving control-plane node")
		survivors := expectNodeIDs(ctx, test, controlPlanePool.ID)
		Expect(survivors).To(HaveKey(replacement.ID), "the staged replacement must be the surviving control-plane node")
		Expect(survivors).NotTo(HaveKey(original), "the rotated-out node must be gone")
	}, Label("lifecycle", "cp-rotation", "scale"), NodeTimeout(2*time.Hour))
}
