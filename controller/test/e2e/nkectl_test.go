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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/NVIDIA/nke/controller/test/e2e/internal/nke"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/nkectl"
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
		Kubeconfig: t.cfg.Kube.Kubeconfig,
		Context:    t.cfg.Kube.Context,
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

// cleanupNkectlCreated removes any cluster nkectl created that still exists,
// including one the suite never recorded because its post-create lookup failed.
// A no-op for entries that did not use the CLI.
func (t *tenantTest) cleanupNkectlCreated(ctx context.Context) {
	if t.nkectlRunner == nil {
		return
	}
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
