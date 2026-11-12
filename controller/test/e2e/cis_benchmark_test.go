// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
	"k8s.io/client-go/kubernetes"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kubebench"
)

const (
	// cisBenchmarkTimeout is the spec's backstop: the scanner's own timeout
	// ends a scan first, and the spec then needs time to clean up.
	cisBenchmarkTimeout = 20 * time.Minute

	// cisTenantScanTimeout bounds a tenant scan: pulling the kube-bench image
	// on every node and running it takes well under a minute when the pull
	// works. It is short because the CAPD core leg has 30 minutes in all, and
	// a stuck pull must leave time for the leg's diagnostics.
	cisTenantScanTimeout = 5 * time.Minute

	// cisSiteLabel selects the site scan. The bootstrap lane runs the suite
	// with it against the site cluster; validationLabelFilter must never
	// select it, because its pods are privileged and share the host's PID
	// namespace, which the validation image's permissions rule out.
	cisSiteLabel = "cis-site"

	// cisSiteScanEnv set to "true" lets the site scan run against the cluster
	// in KUBECONFIG. Unset, the spec skips, so a run that selects every spec
	// does not scan whatever cluster KUBECONFIG names.
	cisSiteScanEnv = "NVKE_CIS_SITE_SCAN"
)

// cisSiteScanRequested reports whether cisSiteScanEnv asks for the site scan.
func cisSiteScanRequested(getenv func(string) string) bool {
	return strings.TrimSpace(getenv(cisSiteScanEnv)) == "true"
}

// The site cluster's scan: the cluster in KUBECONFIG, judged as a site
// cluster. The bootstrap lane runs it in stage 4d of cli/e2e/bootstrap/run.sh.
var _ = Describe("CIS benchmark, site", Label(cisSiteLabel), func() {
	It("passes the CIS benchmark scan", func(ctx context.Context) {
		if !cisSiteScanRequested(os.Getenv) {
			Skip(fmt.Sprintf("%s is not true", cisSiteScanEnv))
		}
		restConfig := expectSucceed(kube.RESTConfig("", ""))
		clientset := expectSucceed(kubernetes.NewForConfig(restConfig))

		outDir := ""
		if artifactsDir != "" {
			outDir = filepath.Join(artifactsDir, "kube-bench")
		}
		expectCISBenchmarkPasses(ctx, kubebench.Scanner{Clientset: clientset}, kubebench.ClusterSite, outDir)
	}, NodeTimeout(cisBenchmarkTimeout))
})

// validateCISBenchmarkPostCreate scans the tenant cluster with kube-bench and
// fails on any finding the exception register does not cover, or any entry it
// no longer needs. It never skips: a runtime skip fails the CAPD lane.
func validateCISBenchmarkPostCreate(test *tenantTest) {
	It("passes the CIS benchmark scan", func(ctx context.Context) {
		restConfig := expectSucceed(test.tenantClients.AdminRESTConfig(ctx, test.cluster.Name))
		clientset := expectSucceed(kubernetes.NewForConfig(restConfig))

		outDir := ""
		if artifactsDir != "" {
			outDir = filepath.Join(artifactsDir, "kube-bench", test.cluster.Name)
		}
		scanner := kubebench.Scanner{Clientset: clientset, Timeout: cisTenantScanTimeout}
		expectCISBenchmarkPasses(ctx, scanner, kubebench.ClusterTenant, outDir)
	}, Label("lifecycle", "post-ready", "cis-benchmark"), NodeTimeout(cisBenchmarkTimeout))
}

// expectCISBenchmarkPasses scans a cluster, logs the summary, and asserts the
// scan passed. The summary is the failure message, so a red spec lists every
// violation and stale entry.
func expectCISBenchmarkPasses(ctx context.Context, scanner kubebench.Scanner, cluster kubebench.Cluster, outDir string) {
	out, err := kubebench.Check(ctx, scanner, cluster, outDir, time.Now())
	Expect(err).NotTo(HaveOccurred(), "scanning the %s cluster with kube-bench", cluster)
	GinkgoWriter.Println(out.Summary())
	if outDir != "" {
		GinkgoWriter.Printf("kube-bench results: %s\n", outDir)
	}
	Expect(out.Passed()).To(BeTrue(), out.Summary())
}
