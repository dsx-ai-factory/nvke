// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Lifecycle fixtures compare settled cluster state and external writes against goldens.

package controllers

import (
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/cluster-api-provider-nico/pkg/test/fixture"
	internalv1 "github.com/dsx-ai-factory/nke/management-engine/apis/v1"
)

func waitForNKECluster(ctx ginkgo.SpecContext, tc *fixture.Case) {
	gomega.Eventually(func(g gomega.Gomega) {
		cluster := &internalv1.NKECluster{}
		g.Expect(tc.Client.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testCluster}, cluster)).To(gomega.Succeed())
		g.Expect(cluster.Status.ObservedGeneration).To(gomega.Equal(cluster.Generation))
		synced := conditions.Get(cluster, internalv1.SyncedCondition)
		g.Expect(synced).NotTo(gomega.BeNil())
		g.Expect(synced.ObservedGeneration).To(gomega.Equal(cluster.Generation))
	}).WithContext(ctx).WithTimeout(timeout).WithPolling(pollInterval).Should(gomega.Succeed())
}

// IMPORTANT: Read docs/writing-tests.md. There is ZERO reason that you should
// have to add or update a case set.
// Represents a CR at generation 1 because reconciliation does not change spec.
var _ = fixture.DescribeCaseSet(controllerCaseSet(
	"NKECluster create reconciliation",
	"nkecluster-create-",
	func(tc *fixture.Case, _ fixture.CaseSet) {
		ginkgo.It("reconciles the initial NKECluster", func(ctx ginkgo.SpecContext) {
			waitForNKECluster(ctx, tc)
		})
	},
))

// IMPORTANT: Read docs/writing-tests.md. There is ZERO reason that you should
// have to add or update a case set.
// Represents a CR at generation 2 after a spec update.
var _ = fixture.DescribeCaseSet(controllerCaseSet(
	"NKECluster update reconciliation",
	"nkecluster-update-",
	func(tc *fixture.Case, _ fixture.CaseSet) {
		ginkgo.It("reconciles the initial NKECluster", func(ctx ginkgo.SpecContext) {
			waitForNKECluster(ctx, tc)
		})
		ginkgo.It("applies the NKECluster update", func(ctx ginkgo.SpecContext) {
			gomega.Expect(tc.PatchObjects(ctx, "input_update.yaml")).To(gomega.Succeed())
		})
		ginkgo.It("reconciles the updated NKECluster", func(ctx ginkgo.SpecContext) {
			waitForNKECluster(ctx, tc)
		})
	},
))

// IMPORTANT: Read docs/writing-tests.md. There is ZERO reason that you should
// have to add or update a case set.
var _ = fixture.DescribeCaseSet(controllerCaseSet(
	"NKECluster delete reconciliation",
	"nkecluster-delete-",
	func(tc *fixture.Case, _ fixture.CaseSet) {
		ginkgo.It("reconciles the initial NKECluster", func(ctx ginkgo.SpecContext) {
			waitForNKECluster(ctx, tc)
		})
		ginkgo.It("deletes the selected objects", func(ctx ginkgo.SpecContext) {
			_, err := tc.DeleteObjects(ctx, "input_delete.yaml")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("reconciles the deleted NKECluster", func(ctx ginkgo.SpecContext) {
			if input, exists := tc.Input("input_cwe.yaml"); exists {
				var config cweFixtureConfig
				gomega.Expect(yaml.UnmarshalStrict([]byte(input), &config)).To(gomega.Succeed())
				if config.DeletePending {
					gomega.Eventually(func(g gomega.Gomega) {
						cluster := &internalv1.NKECluster{}
						g.Expect(tc.Client.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testCluster}, cluster)).To(gomega.Succeed())
						synced := conditions.Get(cluster, internalv1.SyncedCondition)
						g.Expect(synced).NotTo(gomega.BeNil())
						g.Expect(cluster.Generation).To(gomega.Equal(int64(2)))
						g.Expect(synced.ObservedGeneration).To(gomega.Equal(cluster.Generation))
					}).WithContext(ctx).WithTimeout(timeout).WithPolling(pollInterval).Should(gomega.Succeed())
					return
				}
			}
			gomega.Eventually(func() bool {
				err := tc.Client.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testCluster}, &internalv1.NKECluster{})
				return apierrors.IsNotFound(err)
			}).WithContext(ctx).WithTimeout(timeout).WithPolling(pollInterval).Should(gomega.BeTrue())
		})
	},
))
