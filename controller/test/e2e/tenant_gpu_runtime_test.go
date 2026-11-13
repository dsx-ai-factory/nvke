// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

var tenantGPURuntime bool

func init() {
	flag.BoolVar(&tenantGPURuntime, "e2e.tenant-gpu-runtime", false,
		"install the GPU runtime on a GPU tenant with aicr, run aicr's deployment checks, and run the tenant GPU checks against it")
}

const (
	// aicrDataDir is the --data directory, relative to this package, holding
	// the overlay that takes the InfiniBand fabric out of AICR's NVKE leaf.
	aicrDataDir = "aicr"
	// aicrService is AICR's service for NVKE tenants. A snapshot would infer
	// it from the nodes' nico:// provider IDs; naming it keeps the recipe
	// rendering offline.
	aicrService = "nvke"
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
	// aicrOutputBytes bounds what a command keeps of its output for that
	// message: room for aicrOutputLines lines of any sensible length.
	aicrOutputBytes = 64 << 10
	// aicrKillGrace is how long a canceled command's process group has
	// between SIGTERM and SIGKILL.
	aicrKillGrace = 5 * time.Second
	// aicrWaitDelay bounds the wait for a canceled command's output pipe to
	// close once its process group has been signaled.
	aicrWaitDelay = aicrKillGrace + 5*time.Second
)

// aicrAccelerators maps the run's gpu capability to the AICR accelerator
// with an NVKE Ubuntu training leaf. A GPU missing here has no recipe to
// install.
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

func planTenantGPURuntime(enabled bool, capabilities map[string]string, capabilitiesErr error) tenantGPURuntimePlan {
	if !enabled {
		return tenantGPURuntimePlan{}
	}
	if capabilitiesErr != nil {
		return tenantGPURuntimePlan{skip: "the run's capabilities could not be read: " + capabilitiesErr.Error()}
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
// A shape with no worker pools is a hardware tenant, whose builder makes
// tenantWorkers workers instead.
func (p tenantGPURuntimePlan) tenantSettings(settings tenantValidateSettings, shape clusterShape, tenantWorkers int) tenantValidateSettings {
	if !p.installs() {
		return settings
	}
	workers := tenantWorkers
	if len(shape.workerPools) > 0 {
		workers = 0
		for _, pool := range shape.workerPools {
			workers += pool.DesiredNodeCount
		}
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
		runErr := run.try(ctx, run.dir, "aicr", "validate", "--recipe", run.recipe, "--data", run.data,
			"--phase", "deployment", "--fail-on-error", "--output", report)
		summary, err := aicrValidateResult(runErr, report)
		AddReportEntry("aicr deployment checks", summary)
		Expect(err).NotTo(HaveOccurred(), "aicr's deployment checks should pass")
	}, labels, NodeTimeout(aicrValidateTimeout))
}

// aicrValidateResult is the deployment checks' outcome: the command's exit
// and a report that lists at least one check. aicr exits 0 for a phase with
// no checks, so a recipe that lost them would otherwise pass.
func aicrValidateResult(runErr error, report string) (string, error) {
	summary, readErr := summarizeCTRF(report)
	if readErr != nil {
		summary = "no report: " + readErr.Error()
		readErr = fmt.Errorf("aicr validate should report at least one deployment check: %w", readErr)
	}
	return summary, errors.Join(runErr, readErr)
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
	expiresAt, known := kube.KubeconfigTokenExpiry(kubeconfig)
	deadline, hasDeadline := ctx.Deadline()
	if warning := aicrTokenWarning(expiresAt, known, deadline, hasDeadline); warning != "" {
		AddReportEntry("aicr admin token", warning)
		GinkgoWriter.Println("WARNING: " + warning)
	}
	run.kubeconfig = filepath.Join(run.dir, "kubeconfig")
	Expect(os.WriteFile(run.kubeconfig, kubeconfig, 0o600)).To(Succeed())

	By("Rendering the AICR recipe for " + plan.accelerator)
	run.recipe = filepath.Join(run.dir, "recipe.yaml")
	run.aicr(ctx, aicrRecipeArgs(plan.accelerator, run.data, run.recipe)...)
	return run
}

func aicrRecipeArgs(accelerator, data, output string) []string {
	return []string{
		"recipe", "--service", aicrService, "--accelerator", accelerator, "--os", "ubuntu",
		"--intent", "training", "--data", data, "--output", output,
	}
}

func (r *aicrRun) aicr(ctx context.Context, args ...string) {
	r.command(ctx, r.dir, "aicr", args...)
}

func (r *aicrRun) command(ctx context.Context, dir, name string, args ...string) {
	Expect(r.try(ctx, dir, name, args...)).To(Succeed())
}

// aicrTokenWarning says when the admin token a run wrote expires before the
// spec's deadline, and is empty when it does not or there is no telling. It
// warns rather than fails: the cap may be on one run and not the next, and an
// install usually finishes well inside its limit.
func aicrTokenWarning(expiresAt time.Time, known bool, deadline time.Time, hasDeadline bool) string {
	if !known || !hasDeadline || !expiresAt.Before(deadline) {
		return ""
	}
	return fmt.Sprintf("the tenant admin token expires at %s, %s before this spec's deadline; "+
		"a command still running then gets 401s from the tenant. SPIRE caps a token at its JWT "+
		"signing key's remaining lifetime, and the DSX exchange sets its own.",
		expiresAt.UTC().Format(time.RFC3339), deadline.Sub(expiresAt).Round(time.Second))
}

// try runs a command against the tenant and returns an error carrying the
// end of its output. The environment is built rather than inherited: the
// runner's carries the lane's secrets, which nothing run here needs.
func (r *aicrRun) try(ctx context.Context, dir, name string, args ...string) error {
	out := &tailBuffer{limit: aicrOutputBytes}
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + r.dir,
		"TMPDIR=" + r.dir,
		"KUBECONFIG=" + r.kubeconfig,
	}
	command.Stdout = io.MultiWriter(GinkgoWriter, out)
	command.Stderr = command.Stdout
	// deploy.sh starts helm and kubectl, which hold the output pipe. Killing
	// only bash on timeout would leave them installing into a tenant being
	// torn down, and Run waiting on them; every group in its tree goes
	// instead. AICR's CRD installer runs its step under GNU timeout, which
	// moves into a group of its own, out of reach of the command's.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		// ctx is done by now; the listing needs one that is not.
		groups := processGroups(context.WithoutCancel(ctx), command.Process.Pid)
		signal := func(sig syscall.Signal) {
			for _, group := range groups {
				_ = syscall.Kill(-group, sig)
			}
		}
		signal(syscall.SIGTERM)
		time.AfterFunc(aicrKillGrace, func() { signal(syscall.SIGKILL) })
		return nil
	}
	command.WaitDelay = aicrWaitDelay
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, lastLines(out.String(), aicrOutputLines))
	}
	return nil
}

// processGroups lists the process groups in root's tree, root's own first.
// It is read once, before anything is signaled: a process whose parent dies
// is reparented and drops out of the tree. The suite's own group is never
// listed, so a descendant that joined it cannot take the suite down too.
func processGroups(ctx context.Context, root int) []int {
	groups := []int{root}
	procs, err := listProcesses(ctx)
	if err != nil {
		return groups
	}
	children := map[int][]process{}
	for _, p := range procs {
		children[p.ppid] = append(children[p.ppid], p)
	}
	seen := map[int]bool{root: true, syscall.Getpgrp(): true}
	for queue := []int{root}; len(queue) > 0; queue = queue[1:] {
		for _, child := range children[queue[0]] {
			if !seen[child.pgid] && child.pgid > 1 {
				seen[child.pgid] = true
				groups = append(groups, child.pgid)
			}
			queue = append(queue, child.pid)
		}
	}
	return groups
}

type process struct{ pid, ppid, pgid int }

// listProcesses reads /proc where there is one, since minimal Linux images
// ship without ps, and asks ps elsewhere.
func listProcesses(ctx context.Context) ([]process, error) {
	if entries, err := os.ReadDir("/proc"); err == nil {
		var procs []process
		for _, entry := range entries {
			stat, readErr := os.ReadFile("/proc/" + entry.Name() + "/stat")
			if readErr != nil {
				continue
			}
			// The command name is parenthesized and may itself hold spaces
			// or parentheses; the fields after it do not.
			s := string(stat)
			fields := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
			if len(fields) < 3 {
				continue
			}
			if p, ok := parseProcess(entry.Name(), fields[1], fields[2]); ok {
				procs = append(procs, p)
			}
		}
		if len(procs) > 0 {
			return procs, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, aicrKillGrace)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=", "-o", "ppid=", "-o", "pgid=").Output()
	if err != nil {
		return nil, err
	}
	var procs []process
	for line := range strings.Lines(string(out)) {
		if fields := strings.Fields(line); len(fields) == 3 {
			if p, ok := parseProcess(fields[0], fields[1], fields[2]); ok {
				procs = append(procs, p)
			}
		}
	}
	return procs, nil
}

func parseProcess(pid, ppid, pgid string) (process, bool) {
	var p process
	var err1, err2, err3 error
	p.pid, err1 = strconv.Atoi(pid)
	p.ppid, err2 = strconv.Atoi(ppid)
	p.pgid, err3 = strconv.Atoi(pgid)
	return p, errors.Join(err1, err2, err3) == nil
}

// tailBuffer keeps the last limit bytes written to it. A 45 minute install
// writes far more than a failure message carries, and GinkgoWriter already
// has the whole of it.
type tailBuffer struct {
	buf     []byte
	limit   int
	dropped bool
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.limit; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
		b.dropped = true
	}
	return len(p), nil
}

// String is what the buffer kept, less a first line cut by the limit.
func (b *tailBuffer) String() string {
	s := string(b.buf)
	if b.dropped {
		if _, rest, ok := strings.Cut(s, "\n"); ok {
			return rest
		}
	}
	return s
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
