// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/sync/errgroup"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
)

// parallelClusterStagger spaces out create/delete requests so the clusters'
// lifecycles overlap rather than run one-at-a-time, mirroring the nke-e2e
// default-cluster flow: the controller and the infrastructure must handle several
// concurrent tenant-cluster lifecycles at once.
const parallelClusterStagger = time.Minute

// Ports the nke-e2e "Tenant Cluster Lifecycle" default-cluster flow: create a
// set of tenant clusters with staggered requests, wait for all to be
// provisioned in parallel, then delete them and wait for all to be gone in
// parallel. The table-driven specs elsewhere run one cluster per Ordered entry
// (sequential), so this is the only coverage of concurrent multi-cluster
// lifecycle.
//
// nke-e2e used three fixed clusters differing by CNI (Calico 3.30.1, Calico
// 3.31.4, OVN). The controller's API has no CNI-version field, so the Calico version
// matrix cannot be reproduced here; CNI-plugin diversity (OVN + Calico) is used
// instead. Opt in with the "parallel-lifecycle" label — it is the most
// expensive scenario (multiple clusters provisioning at once).
var _ = Describe("Parallel tenant cluster lifecycle", Ordered, ContinueOnFailure,
	Label("nico-vms", "extended", "parallel-lifecycle"), func() {
		shapes := []clusterShape{
			{
				// Needs an image carrying OVN debs; one built here has none.
				cniPlugin:         CNIPluginOVN,
				controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: nicoVMSLargeInstanceID}},
				workerPools:       []nvke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: nicoVMSLargeInstanceID}},
			},
			{
				cniPlugin:         CNIPluginCalico,
				controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: nicoVMSLargeInstanceID}},
				workerPools:       []nvke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: nicoVMSLargeInstanceID}},
			},
		}
		timeouts := nvke.Options{Provision: 90 * time.Minute, Delete: 10 * time.Minute, Poll: 10 * time.Second}

		var (
			test     tenantTest
			clusters []*api.Cluster
		)

		BeforeAll(func(ctx context.Context) {
			test = expectSucceed(setupTenantTest(ctx, expectSucceed(configuredManagement()), timeouts))
		})

		AfterAll(func(ctx context.Context) {
			// Defensive teardown: the delete spec clears the slice on success, so
			// this only fires if a cluster was created but not deleted. Best effort.
			for _, cluster := range clusters {
				if cluster == nil {
					continue
				}
				_ = test.nvke.DeleteCluster(ctx, cluster.ID)
			}
			for _, cluster := range clusters {
				if cluster == nil {
					continue
				}
				_ = test.nvke.WaitForClusterDeleted(ctx, cluster.ID)
			}
		}, NodeTimeout(time.Hour))

		It("creates the clusters with staggered requests and waits for all provisioned in parallel", func(ctx context.Context) {
			for i, shape := range shapes {
				if i > 0 {
					By(fmt.Sprintf("Staggering the next create by %s so lifecycles overlap", parallelClusterStagger))
					time.Sleep(parallelClusterStagger)
				}

				parallelShape := shape
				spec := nicoVMSSpec(test.cfg, fmt.Sprintf("%s-par%d", clusterNamePrefix, i), parallelShape)
				spec.Name = fmt.Sprintf("%s-%s", spec.Name, spec.Backend) // CreateCluster appends a timestamp
				spec.ControlPlane.ControlPlaneVIP = expectSucceed(
					test.availableControlPlaneVIP(ctx, preferredControlPlaneVIP(spec.Backend)),
				)

				By(fmt.Sprintf("Creating parallel cluster %d (%s)", i, shape.cniPlugin))
				cluster, err := test.nvke.CreateCluster(ctx, spec)
				Expect(err).NotTo(HaveOccurred(), "should create parallel cluster %d", i)
				clusters = append(clusters, cluster)
				test.log.Info("Created parallel cluster", "name", cluster.Name, "id", cluster.ID, "cni", shape.cniPlugin)
			}

			By("Waiting for all clusters to reach provisioned in parallel")
			group, groupCtx := errgroup.WithContext(ctx)
			for _, cluster := range clusters {
				group.Go(func() error {
					if _, err := test.nvke.WaitForClusterReady(groupCtx, cluster.ID); err != nil {
						return fmt.Errorf("cluster %s: %w", cluster.Name, err)
					}
					return nil
				})
			}
			Expect(group.Wait()).To(Succeed(), "all clusters should reach provisioned")
		}, Label("lifecycle"), NodeTimeout(2*time.Hour))

		It("deletes the clusters with staggered requests and waits for all gone in parallel", func(ctx context.Context) {
			if len(clusters) == 0 {
				Skip("no clusters were created")
			}

			for i, cluster := range clusters {
				if i > 0 {
					By(fmt.Sprintf("Staggering the next delete by %s so teardown overlaps", parallelClusterStagger))
					time.Sleep(parallelClusterStagger)
				}
				By(fmt.Sprintf("Deleting parallel cluster %s", cluster.Name))
				Expect(test.nvke.DeleteCluster(ctx, cluster.ID)).To(Succeed(), "should delete cluster %s", cluster.Name)
			}

			By("Waiting for all clusters to disappear in parallel")
			group, groupCtx := errgroup.WithContext(ctx)
			for _, cluster := range clusters {
				group.Go(func() error {
					if err := test.nvke.WaitForClusterDeleted(groupCtx, cluster.ID); err != nil {
						return fmt.Errorf("cluster %s: %w", cluster.Name, err)
					}
					return nil
				})
			}
			Expect(group.Wait()).To(Succeed(), "all clusters should be deleted")
			clusters = nil
		}, Label("lifecycle"), NodeTimeout(time.Hour))
	})
