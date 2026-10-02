// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// The nkectl half of the tenant table: the wiring that points one entry's
// cluster lifecycle at the CLI, and the validations that need a CLI-created
// cluster to have provisioned first.
//
// Driving the binary lives in internal/nkectl; this is the seam between that
// package and the suite. The node pool commands belong here as they ship.

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/api"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/nke"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/nkectl"
)

// useNkectl points the test's cluster lifecycle at the nkectl binary, and skips
// the spec when there is no such binary to drive.
//
// Only a missing binary skips, because that alone means the environment does not
// run CLI specs. A port-forward that will not start or a failing
// `config set-site` is a regression, and skipping on those would hide one behind
// a green run.
func (t *tenantTest) useNkectl(ctx context.Context, timeouts nke.Options) {
	GinkgoHelper()

	runner, err := nkectl.New(ctx, nkectl.Options{
		Kubeconfig: t.mgmt.Kubeconfig,
		Context:    t.mgmt.Context,
		Timeouts:   timeouts,
		Logf:       func(format string, args ...any) { t.log.Info(fmt.Sprintf(format, args...)) },
	})
	if errors.Is(err, nkectl.ErrBinaryNotFound) {
		Skip("no nkectl binary, so the CLI entry cannot run: " + err.Error())
	}
	Expect(err).NotTo(HaveOccurred(), "should start the nkectl runner")

	t.nkectlRunner = runner
	t.lifecycle = runner
	t.log.Info("Driving the cluster lifecycle through nkectl", "endpoint", runner.BaseURL())
}

// cleanupNkectlCreated removes any node pool and cluster nkectl created that
// still exists,
// including one the suite never recorded because its post-create lookup failed.
// A no-op for entries that did not use the CLI.
func (t *tenantTest) cleanupNkectlCreated(ctx context.Context) {
	if t.nkectlRunner == nil {
		return
	}
	// Pools first: CleanupCreated takes their clusters away.
	t.nkectlRunner.CleanupCreatedNodePools(ctx)
	t.nkectlRunner.CleanupCreated(ctx)
}

// closeNkectl releases the runner's port-forward and throwaway config. It must
// run after any cleanup that still needs the forward.
func (t *tenantTest) closeNkectl() {
	if t.nkectlRunner == nil {
		return
	}
	if err := t.nkectlRunner.Close(); err != nil {
		t.log.Info("Closing the nkectl runner", "error", err)
	}
	t.nkectlRunner = nil
}

// validateNkectlPostCreate registers the CLI specs that need a provisioned
// cluster. initCluster already drove the create, list and provisioning wait
// through nkectl; these are the commands left over.
func validateNkectlPostCreate(test *tenantTest) {
	Describe("nkectl against the provisioned cluster", Label("nkectl"), func() {
		BeforeEach(func() {
			// Assert rather than Skip: a missing runner here is a bug in this
			// file, and a Skip reported from a BeforeEach is invisible to
			// e2e_assert_no_new_skips, so the lane would stay green with
			// neither spec below ever running.
			Expect(test.nkectlRunner).NotTo(BeNil(),
				"the nkectl runner should still be open; it is closed in AfterAll, after these specs")
		})

		// The gap this entry exists to close. `get kubeconfig` reads
		// nke-system/<cluster>-admin-kubeconfig through the site's own
		// kubeconfig rather than the CWE API, and that Secret appears only after
		// provisioning -- which the smoke lane's cluster never does.
		It("prints a usable admin kubeconfig", func(ctx context.Context) {
			raw, err := test.nkectlRunner.GetKubeconfig(ctx, test.cluster.Name)
			Expect(err).NotTo(HaveOccurred(), "should fetch the tenant admin kubeconfig")

			// Assert the shape, not just that bytes came back: an empty Secret
			// value would otherwise pass.
			Expect(string(raw)).To(ContainSubstring("apiVersion:"), "should look like a kubeconfig")
			Expect(string(raw)).To(ContainSubstring("clusters:"), "should name the tenant cluster")

			By("Using the kubeconfig to reach the tenant API server")
			path := filepath.Join(GinkgoT().TempDir(), "tenant.kubeconfig")
			Expect(os.WriteFile(path, raw, 0o600)).To(Succeed())

			restCfg, err := clientcmd.BuildConfigFromFlags("", path)
			Expect(err).NotTo(HaveOccurred(), "the printed kubeconfig should parse")

			clientset, err := kubernetes.NewForConfig(restCfg)
			Expect(err).NotTo(HaveOccurred())

			nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred(), "the printed kubeconfig should authenticate to the tenant cluster")
			Expect(nodes.Items).NotTo(BeEmpty(), "the tenant cluster should report at least one node")
		}, SpecTimeout(5*time.Minute))

		// The CLI's unit tests cover the flag surface; what is left to e2e is a
		// patch reaching CWE and sticking to a cluster that provisioned.
		//
		// enableCPNodeRecycling only changes what a later upgrade would do, so it
		// starts nothing that could collide with the delete below. The one-way
		// fields each hold the update lock for a workflow, and a version bump
		// recycles nodes.
		It("applies a patch to the provisioned cluster", func(ctx context.Context) {
			patched, err := test.nkectlRunner.PatchCluster(ctx, test.cluster.Name,
				[]byte("enableCPNodeRecycling: true\n"))
			Expect(err).NotTo(HaveOccurred(), "should patch the cluster")

			Expect(patched.EnableCPNodeRecycling).NotTo(BeNil(), "the patched field should be reported")
			Expect(*patched.EnableCPNodeRecycling).To(BeTrue(), "the patch should have been applied")
		}, SpecTimeout(5*time.Minute))

		// The smoke asserts this too, but against a CWE holding no clusters. A
		// real cluster exists alongside here, so this also shows the lookup
		// filters rather than simply finding nothing.
		It("reports a missing cluster as not found", func(ctx context.Context) {
			out, err := test.nkectlRunner.Run(ctx, "get", "clusters", test.cluster.Name+"-absent")
			Expect(err).To(HaveOccurred(), "a cluster that does not exist should not resolve")
			Expect(strings.ToLower(out)).To(ContainSubstring("not found"))
		}, SpecTimeout(2*time.Minute))
	})
}

const (
	// nkectlPoolName is the node pool the CLI lifecycle below creates. Distinct
	// from the names nodepool_lifecycle_test.go uses so a shared cluster could
	// never have the two collide.
	nkectlPoolName = "nkectl-workers"
	// nkectlPoolNodeNamePrefix is the pool's node name prefix. Node names are
	// <cluster>-<prefix>-<4 digits> (controller/internal/naming/node.go), which
	// is how the tenant-side readiness assertion finds this pool's nodes: nkectl
	// has no node-level command to list them with.
	nkectlPoolNodeNamePrefix = "nkw"
)

// validateNkectlNodePoolLifecycle drives a node pool through create, scale up,
// scale down and delete with nkectl alone, asserting the nodes it adds become
// Ready in the tenant cluster.
//
// It runs on the CLI rows of both container-backed lanes, so the commands are
// covered against CAPD and against NICo.
func validateNkectlNodePoolLifecycle(test *tenantTest) {
	Describe("nkectl node pool lifecycle", Label("nkectl", "nodepool", "scale"), func() {
		It("creates, scales and deletes a node pool through the CLI", func(ctx context.Context) {
			Expect(test.nkectlRunner).NotTo(BeNil(),
				"the nkectl runner should still be open; it is closed in AfterAll, after these specs")
			ref := nkectl.NodePoolRef{Cluster: test.cluster.Name, Pool: nkectlPoolName}

			By("Creating a worker node pool with `nkectl create nodepool -f`")
			req := expectSucceed(workerNodePoolRequest(test, nkectlPoolName, nkectlPoolNodeNamePrefix))
			pool := expectSucceed(test.nkectlRunner.CreateNodePool(ctx, test.cluster.Name, req))
			Expect(pool.Name).To(Equal(nkectlPoolName))
			Expect(pool.ControlPlane).To(BeFalse(), "the created pool should be a worker pool")
			Expect(pool.DesiredNodeCount).To(Equal(1))
			Expect(pool.ClusterID).To(Equal(test.cluster.ID), "the pool should belong to this cluster")

			By("Waiting for `nkectl get nodepools` to report the pool provisioned")
			expectSucceed(test.nkectlRunner.WaitForNodePoolProvisioned(ctx, ref))

			By("Verifying the pool appears in the cluster's node pool listing")
			listed := expectSucceed(test.nkectlRunner.ListNodePools(ctx, test.cluster.Name))
			Expect(nodePoolNames(listed)).To(ContainElement(nkectlPoolName))

			tenantClient := nkectlTenantClient(ctx, test)
			nodePrefix := fmt.Sprintf("%s-%s-", test.cluster.Name, req.NodeNamePrefix)

			By("Waiting for the pool's node to be Ready in the tenant cluster")
			eventuallyReadyPoolNodes(ctx, test, tenantClient, nodePrefix, 1)

			By("Scaling the pool to two with `nkectl patch nodepool --patch`")
			expectSucceed(test.nkectlRunner.ScaleNodePool(ctx, ref, 2))
			expectSucceed(test.nkectlRunner.WaitForNodePoolDesiredCount(ctx, ref, 2))
			scaledUpNodes := eventuallyReadyPoolNodes(ctx, test, tenantClient, nodePrefix, 2)

			// Scaling down asserts desiredNodeCount, not the node count:
			// controller/internal/controllers/nodeset/controller.go deletes only
			// pending nodes on a surplus and deliberately skips auto-teardown of
			// running ones, so both nodes stay up.
			By("Scaling the pool back to one and verifying the running nodes are kept")
			expectSucceed(test.nkectlRunner.ScaleNodePool(ctx, ref, 1))
			scaledDown := expectSucceed(test.nkectlRunner.WaitForNodePoolDesiredCount(ctx, ref, 1))
			Expect(scaledDown.DesiredNodeCount).To(Equal(1))
			Expect(expectSucceed(readyPoolNodes(ctx, tenantClient, nodePrefix))).To(Equal(scaledUpNodes),
				"scaling down must not tear down running nodes")

			By("Deleting the pool with `nkectl delete nodepool`")
			Expect(test.nkectlRunner.DeleteNodePool(ctx, ref)).To(Succeed())
			Expect(test.nkectlRunner.WaitForNodePoolDeleted(ctx, ref)).To(Succeed())

			By("Verifying the CLI reports the deleted pool as not found")
			out, err := test.nkectlRunner.Run(ctx, "get", "nodepools", nkectlPoolName, "--cluster", test.cluster.Name)
			Expect(err).To(HaveOccurred(), "a deleted node pool should not resolve")
			Expect(strings.ToLower(out)).To(ContainSubstring("not found"))
		}, SpecTimeout(2*time.Hour))
	})
}

// nkectlTenantClient returns a client for the tenant cluster, retrying while
// its credentials are still being published.
func nkectlTenantClient(ctx context.Context, test *tenantTest) client.Client {
	GinkgoHelper()
	var tenantClient client.Client
	Eventually(func(ctx context.Context) error {
		var err error
		tenantClient, err = test.tenantClients.GetClient(ctx, test.cluster.Name)
		return err
	}).WithContext(ctx).WithTimeout(1 * time.Minute).WithPolling(1 * time.Second).Should(Succeed())
	return tenantClient
}

// eventuallyReadyPoolNodes blocks until the tenant cluster reports exactly want
// Ready nodes named for this pool, and returns their names.
func eventuallyReadyPoolNodes(
	ctx context.Context,
	test *tenantTest,
	tenantClient client.Client,
	nodePrefix string,
	want int,
) []string {
	GinkgoHelper()
	var ready []string
	Eventually(func(ctx context.Context) error {
		var err error
		ready, err = readyPoolNodes(ctx, tenantClient, nodePrefix)
		if err != nil {
			return err
		}
		if len(ready) != want {
			return fmt.Errorf("tenant cluster reports %d Ready nodes named %s*, want %d", len(ready), nodePrefix, want)
		}
		return nil
	}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(5 * time.Second).Should(Succeed())
	test.log.Info("Node pool nodes are Ready in the tenant cluster", "prefix", nodePrefix, "nodes", ready)
	return ready
}

// readyPoolNodes returns the sorted names of the tenant cluster's Ready nodes
// whose name carries nodePrefix.
func readyPoolNodes(ctx context.Context, tenantClient client.Client, nodePrefix string) ([]string, error) {
	var nodes corev1.NodeList
	if err := tenantClient.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("listing tenant Kubernetes nodes: %w", err)
	}
	var ready []string
	for i := range nodes.Items {
		if !strings.HasPrefix(nodes.Items[i].Name, nodePrefix) {
			continue
		}
		if isReady, _ := kubernetesNodeReady(&nodes.Items[i]); isReady {
			ready = append(ready, nodes.Items[i].Name)
		}
	}
	sort.Strings(ready)
	return ready, nil
}

func nodePoolNames(pools []api.NodePool) []string {
	names := make([]string, 0, len(pools))
	for _, pool := range pools {
		names = append(names, pool.Name)
	}
	return names
}
