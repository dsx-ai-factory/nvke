// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra"
)

var tenantGPURuntime bool

func init() {
	flag.BoolVar(&tenantGPURuntime, "e2e.tenant-gpu-runtime", false,
		"install the GPU runtime on a GPU tenant with aicr, run aicr's deployment checks, and run the tenant GPU checks against it")
}

const (
	// aicrDataDir is the --data directory, relative to this package, holding
	// the overlays AICR's embedded catalog has no leaf for.
	aicrDataDir = "aicr"
	// aicrUbuntu is the one series AICR's os-ubuntu mixin admits. Its
	// readiness pre-flight fails any other before a check runs.
	aicrUbuntu = "24.04"
	// The install builds the driver on each GPU node before the operator
	// reports Ready, which is most of this.
	aicrInstallTimeout  = 45 * time.Minute
	aicrValidateTimeout = 20 * time.Minute
	// aicrOutputLines is how much of a failed command's output a failure
	// message carries. The whole of it is in the spec's GinkgoWriter log.
	aicrOutputLines = 40
)

// aicrAccelerators maps the run's gpu capability to the AICR accelerator
// whose generic Ubuntu training leaf aicrDataDir or the embedded catalog
// provides. A GPU missing here has no recipe to install.
var aicrAccelerators = map[string]string{"gb200": "gb200"}

// tenantGPURuntimePlan is whether a run installs the GPU runtime on its
// tenant, decided when the tree is built because the tenant GPU checks take
// their node count then.
type tenantGPURuntimePlan struct {
	accelerator string
	// skip says why a run that asked for the runtime does not install it.
	// Empty when it installs, and when it did not ask.
	skip string
}

func (p tenantGPURuntimePlan) installs() bool { return p.accelerator != "" }

func planTenantGPURuntime(enabled bool, capabilities map[string]string) tenantGPURuntimePlan {
	if !enabled {
		return tenantGPURuntimePlan{}
	}
	gpu := capabilities[infra.CapabilityGPU]
	if gpu == "" {
		return tenantGPURuntimePlan{skip: "the run asks for no GPU"}
	}
	accelerator, ok := aicrAccelerators[gpu]
	if !ok {
		return tenantGPURuntimePlan{skip: fmt.Sprintf("there is no AICR recipe for gpu=%s", gpu)}
	}
	if series := capabilities["ubuntu"]; series != aicrUbuntu {
		return tenantGPURuntimePlan{skip: fmt.Sprintf("AICR's os-ubuntu mixin admits Ubuntu %s only; the run is on %q", aicrUbuntu, series)}
	}
	return tenantGPURuntimePlan{accelerator: accelerator}
}

// tenantSettings is the lane's tenant check settings, with the GPU checks
// switched on for every worker when the plan installs a runtime for them.
func (p tenantGPURuntimePlan) tenantSettings(settings tenantValidateSettings, shape clusterShape) tenantValidateSettings {
	if !p.installs() {
		return settings
	}
	workers := 0
	for _, pool := range shape.workerPools {
		workers += pool.DesiredNodeCount
	}
	settings.gpuNodes = &workers
	return settings
}

// validateTenantGPURuntime installs the GPU runtime on the tenant with aicr
// and runs aicr's deployment checks against it. Both specs carry gpu, which
// the lanes without a GPU exclude, and gpu-runtime, which selects them alone.
// Like the tenant checks, they skip once an earlier spec on the tenant failed.
func validateTenantGPURuntime(test *tenantTest, plan tenantGPURuntimePlan, failed func() bool) {
	labels := Label("gpu", "gpu-runtime")
	if !plan.installs() {
		It("installs the GPU runtime with AICR", func() { Skip(plan.skip) }, labels)
		return
	}

	It("installs the GPU runtime with AICR", func(ctx context.Context) {
		skipAfterFailure(failed)
		run := newAICRRun(ctx, test, plan, aicrInstallTimeout)
		By("Bundling the recipe")
		bundle := filepath.Join(run.dir, "bundle")
		run.aicr(ctx, "bundle", "--recipe", run.recipe, "--data", run.data, "--output", bundle)
		By("Deploying the bundle to the tenant")
		run.command(ctx, bundle, "bash", "./deploy.sh")
	}, labels, NodeTimeout(aicrInstallTimeout))

	It("passes AICR's deployment checks", func(ctx context.Context) {
		skipAfterFailure(failed)
		run := newAICRRun(ctx, test, plan, aicrValidateTimeout)
		report := filepath.Join(run.dir, "aicr-validate.ctrf.json")
		By("Running aicr validate --phase deployment")
		err := run.try(ctx, run.dir, "aicr", "validate", "--recipe", run.recipe, "--data", run.data,
			"--phase", "deployment", "--fail-on-error", "--output", report)
		if summary, readErr := summarizeCTRF(report); readErr == nil {
			AddReportEntry("aicr deployment checks", summary)
		} else {
			AddReportEntry("aicr deployment checks", "no report: "+readErr.Error())
		}
		Expect(err).NotTo(HaveOccurred(), "aicr's deployment checks should pass")
	}, labels, NodeTimeout(aicrValidateTimeout))
}

func skipAfterFailure(failed func() bool) {
	if failed() {
		Skip("an earlier check on this tenant failed")
	}
}

// aicrRun is one spec's working directory: a relayed admin kubeconfig for the
// tenant and the recipe rendered for it. Each spec renders its own, so neither
// depends on a file the other left behind; rendering is offline and quick.
type aicrRun struct {
	dir, kubeconfig, data, recipe string
}

func newAICRRun(ctx context.Context, test *tenantTest, plan tenantGPURuntimePlan, lifetime time.Duration) *aicrRun {
	_, err := exec.LookPath("aicr")
	Expect(err).NotTo(HaveOccurred(), "aicr should be on PATH; the workflow installs it when the GPU runtime is asked for")
	data, err := filepath.Abs(aicrDataDir)
	Expect(err).NotTo(HaveOccurred())

	run := &aicrRun{dir: GinkgoT().TempDir(), data: data}
	By("Writing a relayed admin kubeconfig for the tenant")
	// The token outlives the spec, so a long install does not lose its
	// credential partway through.
	kubeconfig := expectSucceed(test.tenantClients.RelayedAdminKubeconfig(ctx, test.cluster.Name, lifetime+5*time.Minute))
	run.kubeconfig = filepath.Join(run.dir, "kubeconfig")
	Expect(os.WriteFile(run.kubeconfig, kubeconfig, 0o600)).To(Succeed())

	By("Rendering the AICR recipe for " + plan.accelerator)
	run.recipe = filepath.Join(run.dir, "recipe.yaml")
	run.aicr(ctx, "recipe", "--service", "generic", "--accelerator", plan.accelerator, "--os", "ubuntu",
		"--intent", "training", "--data", run.data, "--output", run.recipe)
	return run
}

func (r *aicrRun) aicr(ctx context.Context, args ...string) {
	r.command(ctx, r.dir, "aicr", args...)
}

func (r *aicrRun) command(ctx context.Context, dir, name string, args ...string) {
	Expect(r.try(ctx, dir, name, args...)).To(Succeed())
}

// try runs a command against the tenant and returns an error carrying the
// end of its output. The environment is built rather than inherited: the
// runner's carries the lane's secrets, which nothing run here needs.
func (r *aicrRun) try(ctx context.Context, dir, name string, args ...string) error {
	var out bytes.Buffer
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + r.dir,
		"TMPDIR=" + r.dir,
		"KUBECONFIG=" + r.kubeconfig,
	}
	command.Stdout = io.MultiWriter(GinkgoWriter, &out)
	command.Stderr = command.Stdout
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, lastLines(out.String(), aicrOutputLines))
	}
	return nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// summarizeCTRF renders aicr's CTRF report as one line per check, so the
// outcome of each reaches the JUnit report without the report itself, whose
// contents have not been reviewed for what a public artifact may carry.
func summarizeCTRF(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var report struct {
		Results struct {
			Tool struct {
				Version string `json:"version"`
			} `json:"tool"`
			Tests []struct {
				Name    string `json:"name"`
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"tests"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return "", fmt.Errorf("parsing %s: %w", filepath.Base(path), err)
	}
	if len(report.Results.Tests) == 0 {
		return "", fmt.Errorf("%s lists no checks", filepath.Base(path))
	}
	lines := []string{"aicr " + report.Results.Tool.Version}
	for _, test := range report.Results.Tests {
		line := test.Status + "  " + test.Name
		if test.Status != "passed" && test.Message != "" {
			line += ": " + test.Message
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), nil
}
