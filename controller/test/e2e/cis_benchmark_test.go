// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

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

	// cisTenantConfigTimeout bounds getting the tenant's admin config, which
	// reads its kubeconfig from the management cluster and may open a relay to
	// its API server. Unbounded, a stall there would run a scan to
	// cisBenchmarkTimeout and fail it, even the one that only reports.
	cisTenantConfigTimeout = 2 * time.Minute

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

// validateCISBenchmark scans the tenant cluster with kube-bench and fails on any
// finding the exception register does not cover, or any entry for a check
// kube-bench did not report. when says where in the cluster's life the scan
// runs, such as "after creation"; it names the spec and the scan's artifacts
// directory. It never skips: a runtime skip fails the lane.
func validateCISBenchmark(test *tenantTest, when string) {
	It("passes the CIS benchmark scan "+when, func(ctx context.Context) {
		scanner, outDir, err := tenantScanner(ctx, test, when)
		Expect(err).NotTo(HaveOccurred())
		expectCISBenchmarkPasses(ctx, scanner, kubebench.ClusterTenant, outDir)
	}, Label("lifecycle", "post-ready", "cis-benchmark"), NodeTimeout(cisBenchmarkTimeout))
}

// expectCISBenchmarkPasses scans a cluster, logs the summary, and asserts the
// scan passed. The summary is the failure message, so a red spec lists every
// violation and stale entry.
func expectCISBenchmarkPasses(ctx context.Context, scanner kubebench.Scanner, cluster kubebench.Cluster, outDir string) {
	out, err := kubebench.Check(ctx, scanner, cluster, outDir)
	Expect(err).NotTo(HaveOccurred(), "scanning the %s cluster with kube-bench", cluster)
	GinkgoWriter.Println(out.Summary())
	if outDir != "" {
		GinkgoWriter.Printf("kube-bench results: %s\n", outDir)
	}
	Expect(out.Passed()).To(BeTrue(), out.Summary())
}

// reportCISBenchmark scans the tenant cluster with kube-bench and reports what
// it finds without judging the spec by it. It is for components NVKE does not
// own, such as the GPU runtime AICR installs, whose findings are their owners'
// to fix. The summary goes in the spec's report entry and its log, and the
// results under -e2e.artifacts-dir. A scan whose results cannot be written still
// reports its summary, and a scan that cannot run is reported the same way, so
// the scan never fails and never skips the specs after it.
func reportCISBenchmark(test *tenantTest, when string, failed func() bool, labels Labels) {
	It("reports the CIS benchmark scan "+when, func(ctx context.Context) {
		skipAfterFailure(failed)
		entry := "kube-bench " + when
		var out kubebench.Outcome
		scanner, outDir, err := tenantScanner(ctx, test, when)
		if err == nil {
			out, err = kubebench.Check(ctx, scanner, kubebench.ClusterTenant, outDir)
		}
		report := cisScanReport(out, err)
		AddReportEntry(entry, report)
		GinkgoWriter.Printf("%s: %s\n", entry, report)
	}, labels, NodeTimeout(cisBenchmarkTimeout))
}

// cisScanReport is what reportCISBenchmark records for a scan that returned out
// and err: the summary, then the error when only writing the results failed, or
// why the scan did not run.
func cisScanReport(out kubebench.Outcome, err error) string {
	switch {
	case err == nil:
		return out.Summary()
	case errors.Is(err, kubebench.ErrArtifacts):
		return out.Summary() + "The results were not saved: " + err.Error() + "\n"
	default:
		return "the scan did not run: " + err.Error()
	}
}

// tenantScanner is a scanner for the tenant cluster, as the tenant's admin, and
// the directory under -e2e.artifacts-dir for a scan taken when, or "" when no
// artifacts directory is set.
func tenantScanner(ctx context.Context, test *tenantTest, when string) (kubebench.Scanner, string, error) {
	restConfig, err := tenantAdminConfig(ctx, test.tenantClients, test.cluster.Name, cisTenantConfigTimeout)
	if err != nil {
		return kubebench.Scanner{}, "", err
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return kubebench.Scanner{}, "", fmt.Errorf("building the tenant client: %w", err)
	}
	outDir := ""
	if artifactsDir != "" {
		outDir = filepath.Join(artifactsDir, "kube-bench", test.cluster.Name, strings.ReplaceAll(when, " ", "-"))
	}
	return kubebench.Scanner{Clientset: clientset, Timeout: cisTenantScanTimeout}, outDir, nil
}

// tenantAdminConfig is the tenant's admin config, given up on after timeout.
// The relay's tunnel outlives ctx, so the config stays usable after it returns.
func tenantAdminConfig(ctx context.Context, clients *kube.TenantClientFactory, clusterName string, timeout time.Duration) (*rest.Config, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	restConfig, err := clients.AdminRESTConfig(ctx, clusterName)
	if err != nil {
		return nil, fmt.Errorf("getting the tenant's admin config: %w", err)
	}
	return restConfig, nil
}
