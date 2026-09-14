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

// useNkectl points the test's cluster creation at the nkectl binary.
//
// A missing binary skips the entry rather than failing it. Locally that is the
// common case and there is nothing to learn from a red run; CI stages nkectl
// into the runner before the Ginkgo legs start (hack/ci/nkectl-smoke.sh's
// build and stage steps run first), so a skip there means the staging broke and
// the skip reason says so.
func (t *tenantTest) useNkectl(ctx context.Context, timeouts nke.Options) {
	GinkgoHelper()

	runner, err := nkectl.New(ctx, nkectl.Options{
		Kubeconfig: t.cfg.Kube.Kubeconfig,
		Context:    t.cfg.Kube.Context,
		Timeouts:   timeouts,
		Logf:       func(format string, args ...any) { t.log.Info(fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		Skip("nkectl is not usable here, so the CLI entry cannot run: " + err.Error())
	}

	t.nkectlRunner = runner
	t.creator = runner
	t.log.Info("Driving cluster creation through nkectl", "endpoint", runner.BaseURL())
}

// closeNkectl releases the runner's port-forward and throwaway config.
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
			if test.nkectlRunner == nil {
				Skip("the nkectl runner was not started")
			}
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
