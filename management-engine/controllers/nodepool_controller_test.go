// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/cluster-api-provider-nico/pkg/test/fixture"
	internalv1 "github.com/dsx-ai-factory/nke/management-engine/apis/v1"
)

func waitForNodePool(ctx ginkgo.SpecContext, tc *fixture.Case) {
	waitForNKECluster(ctx, tc)
	gomega.Eventually(func(g gomega.Gomega) {
		pool := &internalv1.NodePool{}
		g.Expect(tc.Client.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testNodePool}, pool)).To(gomega.Succeed())
		g.Expect(pool.Status.ObservedGeneration).To(gomega.Equal(pool.Generation))
		synced := conditions.Get(pool, internalv1.SyncedCondition)
		g.Expect(synced).NotTo(gomega.BeNil())
		g.Expect(synced.ObservedGeneration).To(gomega.Equal(pool.Generation))
	}).WithContext(ctx).WithTimeout(timeout).WithPolling(pollInterval).Should(gomega.Succeed())
}

// IMPORTANT: Read docs/writing-tests.md. There is ZERO reason that you should
// have to add or update a case set.
// Represents a CR at generation 1 because reconciliation does not change spec.
var _ = fixture.DescribeCaseSet(controllerCaseSet(
	"NodePool create reconciliation",
	"nodepool-create-",
	func(tc *fixture.Case, _ fixture.CaseSet) {
		ginkgo.It("reconciles the initial NodePool", func(ctx ginkgo.SpecContext) {
			waitForNodePool(ctx, tc)
		})
	},
))

// IMPORTANT: Read docs/writing-tests.md. There is ZERO reason that you should
// have to add or update a case set.
// Represents a CR at generation 2 after a spec update.
var _ = fixture.DescribeCaseSet(controllerCaseSet(
	"NodePool update reconciliation",
	"nodepool-update-",
	func(tc *fixture.Case, _ fixture.CaseSet) {
		ginkgo.It("reconciles the initial NodePool", func(ctx ginkgo.SpecContext) {
			waitForNodePool(ctx, tc)
		})
		ginkgo.It("applies the NodePool update", func(ctx ginkgo.SpecContext) {
			gomega.Expect(tc.PatchObjects(ctx, "input_update.yaml")).To(gomega.Succeed())
		})
		ginkgo.It("reconciles the updated NodePool", func(ctx ginkgo.SpecContext) {
			waitForNodePool(ctx, tc)
		})
	},
))

// IMPORTANT: Read docs/writing-tests.md. There is ZERO reason that you should
// have to add or update a case set.
var _ = fixture.DescribeCaseSet(controllerCaseSet(
	"NodePool delete reconciliation",
	"nodepool-delete-",
	func(tc *fixture.Case, _ fixture.CaseSet) {
		ginkgo.It("reconciles the initial NodePool", func(ctx ginkgo.SpecContext) {
			waitForNodePool(ctx, tc)
		})
		ginkgo.It("deletes the selected objects", func(ctx ginkgo.SpecContext) {
			_, err := tc.DeleteObjects(ctx, "input_delete.yaml")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("reconciles the deleted NodePool", func(ctx ginkgo.SpecContext) {
			gomega.Eventually(func() bool {
				err := tc.Client.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testNodePool}, &internalv1.NodePool{})
				return apierrors.IsNotFound(err)
			}).WithContext(ctx).WithTimeout(timeout).WithPolling(pollInterval).Should(gomega.BeTrue())
		})
	},
))
