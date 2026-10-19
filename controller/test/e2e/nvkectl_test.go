// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// The nvkectl half of the tenant table: the wiring that points one entry's
// cluster lifecycle at the CLI, and the validations that need a CLI-created
// cluster to have provisioned first.
//
// Driving the binary lives in internal/nvkectl; this is the seam between that
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

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvkectl"
)

// useNvkectl points the test's cluster lifecycle at the nvkectl binary, and skips
// the spec when there is no such binary to drive.
//
// Only a missing binary skips, because that alone means the environment does not
// run CLI specs. A port-forward that will not start or a failing
// `config set-site` is a regression, and skipping on those would hide one behind
// a green run.
func (t *tenantTest) useNvkectl(ctx context.Context, timeouts nvke.Options) {
	GinkgoHelper()

	runner, err := nvkectl.New(ctx, nvkectl.Options{
		Kubeconfig: t.mgmt.Kubeconfig,
		Context:    t.mgmt.Context,
		Timeouts:   timeouts,
		Logf:       func(format string, args ...any) { t.log.Info(fmt.Sprintf(format, args...)) },
	})
	if errors.Is(err, nvkectl.ErrBinaryNotFound) {
		Skip("no nvkectl binary, so the CLI entry cannot run: " + err.Error())
	}
	Expect(err).NotTo(HaveOccurred(), "should start the nvkectl runner")

	t.nvkectlRunner = runner
	t.lifecycle = runner
	t.log.Info("Driving the cluster lifecycle through nvkectl", "endpoint", runner.BaseURL())
}

// cleanupNvkectlCreated removes any node pool and cluster nvkectl created that
// still exists,
// including one the suite never recorded because its post-create lookup failed.
// A no-op for entries that did not use the CLI.
func (t *tenantTest) cleanupNvkectlCreated(ctx context.Context) {
	if t.nvkectlRunner == nil {
		return
	}
	// Pools first: CleanupCreated takes their clusters away.
	t.nvkectlRunner.CleanupCreatedNodePools(ctx)
	t.nvkectlRunner.CleanupCreated(ctx)
}

// closeNvkectl releases the runner's port-forward and throwaway config. It must
// run after any cleanup that still needs the forward.
func (t *tenantTest) closeNvkectl() {
	if t.nvkectlRunner == nil {
		return
	}
	if err := t.nvkectlRunner.Close(); err != nil {
		t.log.Info("Closing the nvkectl runner", "error", err)
	}
	t.nvkectlRunner = nil
}

// validateNvkectlPostCreate registers the CLI specs that need a provisioned
// cluster. initCluster already drove the create, list and provisioning wait
// through nvkectl; these are the commands left over.
func validateNvkectlPostCreate(test *tenantTest) {
	Describe("nvkectl against the provisioned cluster", Label("nvkectl"), func() {
		BeforeEach(func() {
			// Assert rather than Skip: a missing runner here is a bug in this
			// file, and a Skip reported from a BeforeEach is invisible to
			// e2e_assert_no_new_skips, so the lane would stay green with
			// neither spec below ever running.
			Expect(test.nvkectlRunner).NotTo(BeNil(),
				"the nvkectl runner should still be open; it is closed in AfterAll, after these specs")
		})

		// The gap this entry exists to close. `get kubeconfig` reads
		// nvke-system/<cluster>-admin-kubeconfig through the site's own
		// kubeconfig rather than the controller API, and that Secret appears only after
		// provisioning -- which the smoke lane's cluster never does.
		It("prints a usable admin kubeconfig", func(ctx context.Context) {
			raw, err := test.nvkectlRunner.GetKubeconfig(ctx, test.cluster.Name)
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
		// patch reaching the controller and sticking to a cluster that provisioned.
		//
		// enableCPNodeRecycling only changes what a later upgrade would do, so it
		// starts nothing that could collide with the delete below. The one-way
		// fields each hold the update lock for a workflow, and a version bump
		// recycles nodes.
		It("applies a patch to the provisioned cluster", func(ctx context.Context) {
			patched, err := test.nvkectlRunner.PatchCluster(ctx, test.cluster.Name,
				[]byte("enableCPNodeRecycling: true\n"))
			Expect(err).NotTo(HaveOccurred(), "should patch the cluster")

			Expect(patched.EnableCPNodeRecycling).NotTo(BeNil(), "the patched field should be reported")
			Expect(*patched.EnableCPNodeRecycling).To(BeTrue(), "the patch should have been applied")
		}, SpecTimeout(5*time.Minute))

		// The smoke asserts this too, but against a controller holding no clusters. A
		// real cluster exists alongside here, so this also shows the lookup
		// filters rather than simply finding nothing.
		It("reports a missing cluster as not found", func(ctx context.Context) {
			out, err := test.nvkectlRunner.Run(ctx, "get", "clusters", test.cluster.Name+"-absent")
			Expect(err).To(HaveOccurred(), "a cluster that does not exist should not resolve")
			Expect(strings.ToLower(out)).To(ContainSubstring("not found"))
		}, SpecTimeout(2*time.Minute))
	})
}

const (
	// nvkectlPoolName is the node pool the CLI lifecycle below creates. Distinct
	// from the names nodepool_lifecycle_test.go uses so a shared cluster could
	// never have the two collide.
	nvkectlPoolName = "nvkectl-workers"
	// nvkectlPoolNodeNamePrefix is the pool's node name prefix. Node names are
	// <cluster>-<prefix>-<4 digits> (controller/internal/naming/node.go), which
	// is how the tenant-side readiness assertion finds this pool's nodes: nvkectl
	// has no node-level command to list them with.
	nvkectlPoolNodeNamePrefix = "nkw"
	// nvkectlPoolLabelKey and nvkectlPoolTaintKey are the label and taint the pool
	// is created with, to read back through the CLI. They sit outside the
	// namespaces the controller reserves for itself.
	nvkectlPoolLabelKey = "example.com/nvkectl-e2e"
	nvkectlPoolTaintKey = "example.com/nvkectl-e2e"
)

// validateNvkectlNodePoolLifecycle drives a node pool through create, scale up,
// scale down and delete with nvkectl alone, asserting the nodes it adds become
// Ready in the tenant cluster.
//
// It runs on the CLI rows of both container-backed lanes, so the commands are
// covered against CAPD and against NICo.
func validateNvkectlNodePoolLifecycle(test *tenantTest) {
	Describe("nvkectl node pool lifecycle", Label("nvkectl", "nodepool", "scale"), func() {
		It("creates, scales and deletes a node pool through the CLI", func(ctx context.Context) {
			Expect(test.nvkectlRunner).NotTo(BeNil(),
				"the nvkectl runner should still be open; it is closed in AfterAll, after these specs")
			ref := nvkectl.NodePoolRef{Cluster: test.cluster.Name, Pool: nvkectlPoolName}

			By("Creating a worker node pool with `nvkectl create nodepool -f`")
			req := expectSucceed(workerNodePoolRequest(test, nvkectlPoolName, nvkectlPoolNodeNamePrefix))
			wantLabels := map[string]string{nvkectlPoolLabelKey: "true"}
			wantTaints := []api.NodeTaint{{Key: nvkectlPoolTaintKey, Value: "true", Effect: api.NodeTaintEffectNoSchedule}}
			req.NodeLabels = wantLabels
			req.NodeTaints = wantTaints
			pool := expectSucceed(test.nvkectlRunner.CreateNodePool(ctx, test.cluster.Name, req))
			Expect(pool.Name).To(Equal(nvkectlPoolName))
			Expect(pool.NodeLabels).To(Equal(wantLabels), "`nvkectl get nodepools -o json` should report the labels the pool was created with")
			Expect(pool.NodeTaints).To(Equal(wantTaints), "`nvkectl get nodepools -o json` should report the taints the pool was created with")
			Expect(pool.ControlPlane).To(BeFalse(), "the created pool should be a worker pool")
			Expect(pool.DesiredNodeCount).To(Equal(1))
			Expect(pool.ClusterID).To(Equal(test.cluster.ID), "the pool should belong to this cluster")

			By("Waiting for `nvkectl get nodepools` to report the pool provisioned")
			expectSucceed(test.nvkectlRunner.WaitForNodePoolProvisioned(ctx, ref))

			By("Verifying the pool appears in the cluster's node pool listing")
			listed := expectSucceed(test.nvkectlRunner.ListNodePools(ctx, test.cluster.Name))
			Expect(nodePoolNames(listed)).To(ContainElement(nvkectlPoolName))

			tenantClient := nvkectlTenantClient(ctx, test)
			nodePrefix := fmt.Sprintf("%s-%s-", test.cluster.Name, req.NodeNamePrefix)

			By("Waiting for the pool's node to be Ready in the tenant cluster")
			eventuallyReadyPoolNodes(ctx, test, tenantClient, nodePrefix, 1)

			By("Scaling the pool to two with `nvkectl patch nodepool --patch`")
			expectSucceed(test.nvkectlRunner.ScaleNodePool(ctx, ref, 2))
			expectSucceed(test.nvkectlRunner.WaitForNodePoolDesiredCount(ctx, ref, 2))
			scaledUpNodes := eventuallyReadyPoolNodes(ctx, test, tenantClient, nodePrefix, 2)

			// Scaling down asserts desiredNodeCount, not the node count:
			// controller/internal/controllers/nodeset/controller.go deletes only
			// pending nodes on a surplus and deliberately skips auto-teardown of
			// running ones, so both nodes stay up.
			By("Scaling the pool back to one and verifying the running nodes are kept")
			expectSucceed(test.nvkectlRunner.ScaleNodePool(ctx, ref, 1))
			scaledDown := expectSucceed(test.nvkectlRunner.WaitForNodePoolDesiredCount(ctx, ref, 1))
			Expect(scaledDown.DesiredNodeCount).To(Equal(1))
			Expect(expectSucceed(readyPoolNodes(ctx, tenantClient, nodePrefix))).To(Equal(scaledUpNodes),
				"scaling down must not tear down running nodes")

			By("Deleting the pool with `nvkectl delete nodepool`")
			Expect(test.nvkectlRunner.DeleteNodePool(ctx, ref)).To(Succeed())
			Expect(test.nvkectlRunner.WaitForNodePoolDeleted(ctx, ref)).To(Succeed())

			By("Verifying the CLI reports the deleted pool as not found")
			out, err := test.nvkectlRunner.Run(ctx, "get", "nodepools", nvkectlPoolName, "--cluster", test.cluster.Name)
			Expect(err).To(HaveOccurred(), "a deleted node pool should not resolve")
			Expect(strings.ToLower(out)).To(ContainSubstring("not found"))
		}, SpecTimeout(2*time.Hour))
	})
}

// nvkectlTenantClient returns a client for the tenant cluster, retrying while
// its credentials are still being published.
func nvkectlTenantClient(ctx context.Context, test *tenantTest) client.Client {
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
