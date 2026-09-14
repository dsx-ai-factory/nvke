// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// The nkectl half of the tenant table: the wiring that points one entry's
// cluster creation at the CLI, and the validations that only make sense once a
// CLI-created cluster has actually provisioned.
//
// Everything specific to driving the binary lives in internal/nkectl. This file
// is the seam between that package and the suite, kept separate from
// tenant_test.go so the CLI work stays legible as CLI work and has somewhere
// obvious to grow -- `patch cluster` and the node pool commands land here as
// they ship.

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

// useNkectl points the test's cluster lifecycle at the nkectl binary.
//
// Only a missing binary skips. That is the one condition that says "this
// environment does not run CLI specs" -- a developer's checkout without nkectl
// built. Everything else nkectl.New can return (the port-forward not starting,
// `config set-site` failing) is a real failure, and skipping on it would hide a
// CLI regression behind a green run.
//
// The distinction matters in CI both ways: a skip here is reported from an It,
// so e2e_assert_no_new_skips turns the leg red with a "not usable" reason
// rather than the failure itself, while a genuine regression that skipped would
// never be reported at all.
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

// cleanupNkectlCreated removes any cluster nkectl created that is still there.
//
// The suite's own AfterAll covers the cluster it knows about. This covers the
// one it cannot: a create that succeeded but whose follow-up lookup failed,
// which leaves test.cluster nil and the cluster itself running.
func (t *tenantTest) cleanupNkectlCreated(ctx context.Context) {
	if t.nkectlRunner == nil {
		return
	}
	t.nkectlRunner.CleanupCreated(ctx)
}

// closeNkectl releases the runner's port-forward and throwaway config. Called
// from AfterAll, after any cleanup that still needs the forward.
func (t *tenantTest) closeNkectl() {
	if t.nkectlRunner == nil {
		return
	}
	if err := t.nkectlRunner.Close(); err != nil {
		t.log.Info("Closing the nkectl runner", "error", err)
	}
	t.nkectlRunner = nil
}

// validateNkectlPostCreate covers what the CLI can only be asked once its
// cluster is up. The create, list and provisioning wait were already driven
// through nkectl by initCluster; these are the commands that had nowhere else
// to be exercised.
func validateNkectlPostCreate(test *tenantTest) {
	Describe("nkectl against the provisioned cluster", Label("nkectl"), func() {
		BeforeEach(func() {
			// Fail, not Skip. Reaching here without a runner means the create
			// spec built one and something took it away again -- a bug in this
			// file, not a property of the environment. A Skip would also be
			// invisible to e2e_assert_no_new_skips, which only matches skips
			// reported from an It, so the lane would stay green with neither
			// spec below ever running.
			Expect(test.nkectlRunner).NotTo(BeNil(),
				"the nkectl runner should still be open; it is closed in AfterAll, after these specs")
		})

		// The headline gap this entry closes. `get kubeconfig` is the one tenant
		// command that never touches the CWE API: it reads
		// nke-system/<cluster>-admin-kubeconfig through the site's own
		// kubeconfig. That Secret exists only after provisioning, so the smoke
		// lane can reach only its not-found path and this is the sole coverage
		// of the success path anywhere.
		It("prints a usable admin kubeconfig", func(ctx context.Context) {
			raw, err := test.nkectlRunner.GetKubeconfig(ctx, test.cluster.Name)
			Expect(err).NotTo(HaveOccurred(), "should fetch the tenant admin kubeconfig")

			// Assert it is a kubeconfig, not merely that bytes came back: an
			// empty Secret value would otherwise pass.
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

		// A name that does not exist must fail with a message that says so.
		// The smoke asserts this too, but against a CWE with no clusters at
		// all; here a real cluster exists alongside, so this also proves the
		// lookup is filtering rather than simply finding nothing.
		It("reports a missing cluster as not found", func(ctx context.Context) {
			out, err := test.nkectlRunner.Run(ctx, "get", "clusters", "-c", test.cluster.Name+"-absent")
			Expect(err).To(HaveOccurred(), "a cluster that does not exist should not resolve")
			Expect(strings.ToLower(out)).To(ContainSubstring("not found"))
		}, SpecTimeout(2*time.Minute))
	})
}
