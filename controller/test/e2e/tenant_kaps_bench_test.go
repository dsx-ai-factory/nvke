// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/site"
)

const (
	// kapsBenchTimeout bounds one run spec, and the token it is given outlasts it.
	kapsBenchTimeout = 30 * time.Minute
	// kapsBenchPrepareTimeout is the first run spec's extra time, in which kaps-bench installs Prometheus.
	kapsBenchPrepareTimeout = 15 * time.Minute
	// kapsBenchStopGrace is kaps-bench's time after SIGTERM to remove an unfinished install.
	kapsBenchStopGrace = 5 * time.Minute
	// kapsBenchResults is written next to qualify-e2e.log, so qualify local collects it with the run.
	kapsBenchResults = "kaps-bench-results.tar.gz"
)

// kapsBenchSetup is the one setup a tenant's run specs share.
type kapsBenchSetup struct {
	attempted bool
	// work holds the cluster context and kaps-bench's output dir, shared by every run spec.
	work string
	err  error
}

// validateTenantKapsBench runs each kaps-bench run spec on the tenant as its own spec.
func validateTenantKapsBench(test *tenantTest, failed func() bool) {
	labels := Label("kaps-bench")
	var setup kapsBenchSetup
	for i, spec := range kapsBenchRunSpecs {
		It("runs kaps-bench run spec "+spec, func(ctx context.Context) {
			if i == len(kapsBenchRunSpecs)-1 {
				// Only the last run spec removes the work dir every run spec shares, so it goes at most once.
				DeferCleanup(func() { _ = os.RemoveAll(setup.work) })
			}
			if reason := setup.skipReason(failed()); reason != "" {
				Skip(reason)
			}
			work, err := setup.ensure(func() (string, error) {
				var dir string
				err := InterceptGomegaFailure(func() { dir = setUpKapsBench(ctx, test) })
				return dir, err
			})
			Expect(err).NotTo(HaveOccurred(), "kaps-bench should be set up for the tenant")
			// Rewrites the archive after every run spec, so an interrupted run keeps what finished.
			DeferCleanup(func() {
				Expect(keepKapsBenchResults(work, kapsBenchResults, CurrentSpecReport().Failed(), test.mgmt.Secrets...)).To(Succeed(),
					"kaps-bench's results should be archived")
			})
			tools := newKapsBenchToolRun(ctx, test, i)
			runCtx, cancel := context.WithDeadline(ctx, CurrentSpecReport().StartTime.Add(kapsBenchRunTimeout(i)))
			defer cancel()
			args := kapsBenchRunArgs(spec, tools.kubeconfig, filepath.Join(work, "cluster-context.yaml"), filepath.Join(work, "output"))
			binary := filepath.Join(kapsBenchDir, "kaps-bench")
			Expect(tools.try(runCtx, kapsBenchDir, binary, args...)).To(Succeed(), "kaps-bench run spec %s should pass", spec)
		}, labels, NodeTimeout(kapsBenchSpecTimeout(i)))
	}
}

// ensure runs step on the first call only and returns its work dir and error on every call.
func (p *kapsBenchSetup) ensure(step func() (string, error)) (string, error) {
	if !p.attempted {
		p.attempted = true
		// Stays set if step panics, so no run spec reads a half-written work dir.
		p.err = errors.New("the kaps-bench setup did not finish")
		p.work, p.err = step()
	}
	return p.work, p.err
}

// skipReason returns why a run spec skips, or nothing when it runs.
func (p *kapsBenchSetup) skipReason(earlierFailure bool) string {
	switch {
	case !p.attempted && earlierFailure:
		return "an earlier check on this tenant failed"
	case p.attempted && p.err != nil:
		return "kaps-bench was not set up for the tenant: " + p.err.Error()
	}
	return ""
}

// setUpKapsBench returns a new work dir holding the cluster context kaps-bench records each run under.
func setUpKapsBench(ctx context.Context, test *tenantTest) string {
	Expect(requireKapsBenchTools(kapsBenchRunSpecs, kapsBenchDir)).To(Succeed())

	By("Writing the cluster context kaps-bench records each run under")
	admin := expectSucceed(test.tenantClients.GetAdminClient(ctx, test.cluster.Name))
	var kubeSystem corev1.Namespace
	Expect(admin.Get(ctx, client.ObjectKey{Name: kubeSystemNamespace}, &kubeSystem)).To(Succeed())
	document := expectSucceed(kapsBenchClusterContext(test.cluster.Name, string(kubeSystem.UID)))
	dir := expectSucceed(os.MkdirTemp("", "kaps-bench-"))
	Expect(os.WriteFile(filepath.Join(dir, "cluster-context.yaml"), document, 0o600)).To(Succeed())
	return dir
}

// parseKapsBenchRunSpecs returns the comma separated run specs, refusing an empty one.
func parseKapsBenchRunSpecs(value string) ([]string, error) {
	specs := strings.Split(value, ",")
	if slices.Contains(specs, "") {
		return nil, fmt.Errorf("-e2e.kaps-bench-run-specs %q has an empty run spec", value)
	}
	return specs, nil
}

// requireKapsBenchTools returns an error naming each tool or folder the run specs need but lack.
func requireKapsBenchTools(specs []string, dir string) error {
	if len(specs) == 0 {
		return nil
	}
	var errs []error
	if dir == "" {
		errs = append(errs, errors.New("-e2e.kaps-bench-dir names no kaps-bench folder, so set E2E_KAPS_BENCH_DIR"))
	} else if !filepath.IsAbs(dir) {
		// A run resolves a relative kaps-bench against this folder, not the process directory Stat uses.
		errs = append(errs, fmt.Errorf("-e2e.kaps-bench-dir %s is not an absolute path", dir))
	} else if info, err := os.Stat(dir); err != nil {
		errs = append(errs, fmt.Errorf("-e2e.kaps-bench-dir: %w", err))
	} else if !info.IsDir() {
		errs = append(errs, fmt.Errorf("-e2e.kaps-bench-dir %s is not a folder", dir))
	} else {
		if _, err := exec.LookPath(filepath.Join(dir, "kaps-bench")); err != nil {
			errs = append(errs, fmt.Errorf("kaps-bench folder %s has no kaps-bench to run: %w", dir, err))
		}
		if _, err := exec.LookPath(filepath.Join(dir, "bin", "helm")); err != nil {
			errs = append(errs, fmt.Errorf("kaps-bench folder %s has no bin/helm to run: %w", dir, err))
		}
		for _, spec := range specs {
			if _, err := os.Stat(filepath.Join(dir, "run-specs", spec)); err != nil {
				errs = append(errs, fmt.Errorf("kaps-bench has no run spec %s: %w", spec, err))
			}
		}
	}
	return errors.Join(errs...)
}

// kapsBenchRunArgs returns the arguments that run one run spec and keep its results in outputDir.
func kapsBenchRunArgs(spec, kubeconfig, clusterContext, outputDir string) []string {
	return []string{
		"run", spec,
		"--kubeconfig", kubeconfig,
		"--cluster-context", clusterContext,
		"--output-dir", outputDir,
		"--local-publisher",
	}
}

// kapsBenchClusterContext returns the cluster context kaps-bench records a tenant's runs under.
func kapsBenchClusterContext(name, uid string) ([]byte, error) {
	return yaml.Marshal(map[string]map[string]string{"cluster": {"name": name, "uuid": uid}})
}

// newKapsBenchToolRun returns a temp dir holding a relayed admin kubeconfig whose token outlasts run spec index.
func newKapsBenchToolRun(ctx context.Context, test *tenantTest, index int) *tenantToolRun {
	// Ginkgo's spec context carries no deadline, so this one lets a capped token be reported.
	ctx, cancel := context.WithTimeout(ctx, kapsBenchSpecTimeout(index))
	defer cancel()
	run := newTenantToolRun(ctx, test, kapsBenchTokenLifetime(index), "kaps-bench admin token")
	run.stopGrace = kapsBenchStopGrace
	return run
}

// kapsBenchSpecTimeout returns run spec index's time limit; the first one also installs Prometheus.
func kapsBenchSpecTimeout(index int) time.Duration {
	if index == 0 {
		return kapsBenchTimeout + kapsBenchPrepareTimeout
	}
	return kapsBenchTimeout
}

// kapsBenchTokenLifetime returns how long run spec index's token is asked to last: five minutes past its limit.
func kapsBenchTokenLifetime(index int) time.Duration {
	return kapsBenchSpecTimeout(index) + 5*time.Minute
}

// kapsBenchRunTimeout returns how long spec index runs kaps-bench so its stop ends by the limit.
func kapsBenchRunTimeout(index int) time.Duration {
	stopping := tenantToolRun{stopGrace: kapsBenchStopGrace}
	// aicrKillGrace also bounds the process listing that comes before SIGTERM.
	return kapsBenchSpecTimeout(index) - aicrKillGrace - stopping.waitDelay()
}

// keepKapsBenchResults archives work's output dir into dest, returning the error only when the spec passed.
func keepKapsBenchResults(work, dest string, specFailed bool, secrets ...string) error {
	if work == "" {
		return nil
	}
	err := archiveKapsBenchResults(filepath.Join(work, "output"), dest, secrets...)
	switch {
	case err == nil:
		GinkgoLogr.Info("archived kaps-bench's results", "path", dest)
	case specFailed:
		GinkgoLogr.Error(err, "could not archive kaps-bench's results after the run spec failed")
		return nil
	}
	return err
}

// archiveKapsBenchResults replaces dest with a tar.gz of dir less secrets, keeping dest on failure.
func archiveKapsBenchResults(dir, dest string, secrets ...string) error {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if err := errors.Join(tw.AddFS(os.DirFS(dir)), tw.Close(), gz.Close()); err != nil {
		return fmt.Errorf("archiving %s into %s: %w", dir, dest, err)
	}
	redacted, err := site.RedactArchive(archive.Bytes(), secrets...)
	if err != nil {
		return fmt.Errorf("redacting the archive of %s: %w", dir, err)
	}
	f, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+"-*")
	if err != nil {
		return fmt.Errorf("creating an archive beside %s: %w", dest, err)
	}
	_, err = f.Write(redacted)
	err = errors.Join(err, f.Close())
	if err == nil {
		err = os.Rename(f.Name(), dest)
	}
	if err != nil {
		return errors.Join(fmt.Errorf("archiving %s into %s: %w", dir, dest, err), os.Remove(f.Name()))
	}
	return nil
}
