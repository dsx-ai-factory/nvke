// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
)

func TestPlanTenantGPURuntime(t *testing.T) {
	for _, tc := range []struct {
		name         string
		enabled      bool
		capabilities map[string]string
		err          error
		accelerator  string
		skip         string
	}{
		{name: "off", capabilities: map[string]string{"gpu": "gb200", "ubuntu": "24.04"}},
		{name: "gb200 on 24.04", enabled: true, capabilities: map[string]string{"gpu": "gb200", "ubuntu": "24.04"}, accelerator: "gb200"},
		{name: "no gpu", enabled: true, capabilities: map[string]string{"ubuntu": "24.04"}, skip: "asks for no GPU"},
		{name: "gpu without a recipe", enabled: true, capabilities: map[string]string{"gpu": "l40s", "ubuntu": "24.04"}, skip: "no AICR recipe for gpu=l40s"},
		{name: "26.04", enabled: true, capabilities: map[string]string{"gpu": "gb200", "ubuntu": "26.04"}, skip: `the run is on "26.04"`},
		// A run that names no series is not assumed to be on the one AICR takes.
		{name: "no series", enabled: true, capabilities: map[string]string{"gpu": "gb200"}, skip: `the run is on ""`},
		{name: "no capabilities", enabled: true, skip: "asks for no GPU"},
		{name: "capabilities unreadable", enabled: true, err: errors.New("bad -e2e.capabilities"), skip: "could not be read: bad -e2e.capabilities"},
		{name: "off with capabilities unreadable", err: errors.New("bad -e2e.capabilities")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := planTenantGPURuntime(tc.enabled, tc.capabilities, tc.err)
			if plan.accelerator != tc.accelerator {
				t.Errorf("accelerator = %q, want %q", plan.accelerator, tc.accelerator)
			}
			if tc.skip == "" && plan.skip != "" || !strings.Contains(plan.skip, tc.skip) {
				t.Errorf("skip = %q, want it to contain %q", plan.skip, tc.skip)
			}
		})
	}
}

// TestGPURuntimeMinutesCoverTheSpecs ties the script's allowance, which every
// clock moves out by, to the two specs' limits.
func TestGPURuntimeMinutesCoverTheSpecs(t *testing.T) {
	// The backend E2E lanes run this package as a compiled binary away from the
	// checkout, as TestUnit_SiteNamespacesCoverTheDeployPath notes; only the
	// checkout-based unit lane has the script to compare.
	script, err := os.ReadFile(filepath.Join("..", "..", "..", "release", "hack", "qualify-e2e.sh"))
	if os.IsNotExist(err) {
		t.Skipf("no qualify-e2e.sh beside the test binary: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^gpu_runtime_minutes=(\d+)$`).FindSubmatch(script)
	if match == nil {
		t.Fatal("qualify-e2e.sh sets no gpu_runtime_minutes")
	}
	minutes, err := strconv.Atoi(string(match[1]))
	if err != nil {
		t.Fatal(err)
	}
	if want := int((aicrInstallTimeout + aicrValidateTimeout) / time.Minute); minutes != want {
		t.Errorf("qualify-e2e.sh gpu_runtime_minutes = %d, want the specs' %d", minutes, want)
	}
}

// TestAICRCommandCancelKillsItsChildren checks that a canceled command takes
// the processes it started with it, as deploy.sh's helm would be. The child
// beats into a file rather than being probed by pid: in a container nothing
// may reap it, and a zombie still answers kill -0. The scripts are sh, not
// bash: the local E2E lane runs this binary in an Alpine image with no bash.
//
// The timeout case is AICR's CRD installer: GNU timeout without --foreground
// moves its child into a group of its own, out of reach of a kill sent to the
// command's group alone.
func TestAICRCommandCancelKillsItsChildren(t *testing.T) {
	const loop = `while :; do echo x >> "$0"; sleep 0.05; done`
	for _, tc := range []struct {
		name    string
		timeout bool
		script  string
	}{
		{name: "a background child", script: `(` + loop + `) & wait`},
		// The trailing command keeps sh from exec'ing timeout, which as the
		// group's leader could not leave it.
		{name: "a child under GNU timeout", timeout: true, script: `"$1" 300 sh -c '` + loop + `' "$0"; exit $?`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"-c", tc.script}
			dir := t.TempDir()
			beat := filepath.Join(dir, "beat")
			args = append(args, beat)
			if tc.timeout {
				timeout := gnuTimeout(t)
				// Reaching timeout's group takes the process list; a
				// sandbox that hides it would fail this for its own reason.
				if _, err := listProcesses(t.Context()); err != nil {
					t.Skipf("cannot list processes here: %v", err)
				}
				args = append(args, timeout)
			}
			run := &tenantToolRun{dir: dir}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()

			start := time.Now()
			if err := run.try(ctx, dir, "sh", args...); err == nil {
				t.Fatal("try returned nil for a canceled command")
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second+run.waitDelay() {
				t.Fatalf("try took %s after cancel; it should not wait on the child", elapsed)
			}
			size := func() int64 {
				info, statErr := os.Stat(beat)
				if statErr != nil {
					t.Fatal(statErr)
				}
				return info.Size()
			}
			time.Sleep(200 * time.Millisecond)
			before := size()
			time.Sleep(500 * time.Millisecond)
			if after := size(); after != before {
				t.Fatalf("the child kept running after its command was canceled: %d bytes, then %d", before, after)
			}
		})
	}
}

// TestToolRunStopGrace checks that a canceled command may finish within its stop grace.
func TestToolRunStopGrace(t *testing.T) {
	t.Parallel()
	const loop = `while :; do sleep 0.05; done`
	for _, tc := range []struct {
		name      string
		stopGrace time.Duration
		script    string
		// stop is how long after the cancel try should return.
		stop   time.Duration
		output string
		killed bool
	}{
		{
			name:      "a command that finishes inside a grace longer than the default",
			stopGrace: 10 * time.Second,
			script:    `trap 'sleep 6; echo removed; exit 0' TERM; ` + loop,
			stop:      6 * time.Second,
			output:    "removed",
		},
		{
			name:      "a command that finishes after the default wait delay but inside its grace",
			stopGrace: 15 * time.Second,
			script:    `trap 'sleep 11; echo removed; exit 0' TERM; ` + loop,
			stop:      11 * time.Second,
			output:    "removed",
		},
		{
			name:      "a command still running at the end of its grace",
			stopGrace: time.Second,
			script:    `trap 'echo stopping' TERM; ` + loop,
			stop:      time.Second,
			output:    "stopping",
			killed:    true,
		},
		{
			name:   "no grace set keeps the default",
			script: `trap 'echo stopping' TERM; ` + loop,
			stop:   aicrKillGrace,
			output: "stopping",
			killed: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			t.Log("Doing: a command that traps SIGTERM, canceled after 200ms")
			dir := t.TempDir()
			run := &tenantToolRun{dir: dir, stopGrace: tc.stopGrace}
			const cancelAfter = 200 * time.Millisecond
			ctx, cancel := context.WithTimeout(t.Context(), cancelAfter)
			defer cancel()

			start := time.Now()
			err := run.try(ctx, dir, "sh", "-c", tc.script)
			elapsed := time.Since(start) - cancelAfter

			t.Log("Testing: try returns when the command ends or its grace runs out, not before")
			if elapsed < tc.stop || elapsed > tc.stop+2*time.Second {
				t.Errorf("try returned %s after the cancel, want about %s", elapsed, tc.stop)
			}
			if err == nil {
				t.Fatal("try returned nil for a canceled command")
			}
			t.Log("Testing: the output the command wrote after SIGTERM is kept")
			if !strings.Contains(err.Error(), tc.output) {
				t.Errorf("try error = %q, want the command's %q in it", err, tc.output)
			}
			if killed := strings.Contains(err.Error(), "signal: killed"); killed != tc.killed {
				t.Errorf("try error = %q, killed = %t, want %t", err, killed, tc.killed)
			}
		})
	}
}

// TestToolRunStopGraceKillsWhatOutlivesTheCommand checks that no listed group outlives a canceled command.
func TestToolRunStopGraceKillsWhatOutlivesTheCommand(t *testing.T) {
	t.Parallel()
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// script starts TestToolRunStopGraceHelper as the child, off the output pipe so try never waits on it.
	const script = `trap 'exit 0' TERM; "$0" -test.run='^TestToolRunStopGraceHelper$' -- "$1" "$2" >/dev/null 2>&1 & ` +
		`while :; do sleep 0.05; done`
	for _, tc := range []struct {
		name      string
		stopGrace time.Duration
		ownGroup  bool
	}{
		{name: "a child in the command's group under the default grace"},
		{name: "a child in the command's group under a long grace", stopGrace: time.Minute},
		{name: "a child in a group of its own under the default grace", ownGroup: true},
		{name: "a child in a group of its own under a long grace", stopGrace: time.Minute, ownGroup: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			group := "same"
			if tc.ownGroup {
				if _, err := listProcesses(t.Context()); err != nil {
					t.Skipf("cannot list processes here: %v", err)
				}
				group = "own"
			}
			t.Log("Doing: a command that exits on SIGTERM and leaves a SIGTERM-ignoring child, canceled once it runs")
			dir := t.TempDir()
			beat := filepath.Join(dir, "beat")
			var pid, pgid, commandPgid int
			readRecord := func() error {
				record, err := os.ReadFile(beat + ".pid")
				if err != nil {
					return err
				}
				_, err = fmt.Sscan(string(record), &pid, &pgid, &commandPgid)
				return err
			}
			t.Cleanup(func() {
				if readRecord() == nil {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			run := &tenantToolRun{dir: dir, stopGrace: tc.stopGrace}
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			go func() {
				for ctx.Err() == nil {
					if _, err := os.Stat(beat + ".pid"); err == nil {
						cancel()
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
			}()

			err := run.try(ctx, dir, "sh", "-c", script, helper, beat, group)
			if err != nil && strings.Contains(err.Error(), "signal: killed") {
				t.Fatalf("try error = %q, want the command to exit on its own", err)
			}

			if err := readRecord(); err != nil {
				t.Fatalf("the child wrote no record: %v", err)
			}
			t.Logf("Testing: the child's group is the command's only when asked: child pgid %d, command pgid %d", pgid, commandPgid)
			if (pgid != commandPgid) != tc.ownGroup {
				t.Fatalf("child pgid %d, command pgid %d, want them to differ only for a group of its own", pgid, commandPgid)
			}

			t.Log("Testing: the child is dead right after try returns, whatever is left of the grace")
			size := func() int64 {
				info, statErr := os.Stat(beat)
				if statErr != nil {
					t.Fatal(statErr)
				}
				return info.Size()
			}
			time.Sleep(200 * time.Millisecond)
			before := size()
			time.Sleep(500 * time.Millisecond)
			if after := size(); after != before {
				t.Errorf("the child kept running after try returned: %d bytes, then %d", before, after)
			}
		})
	}
}

// TestToolRunStopGraceHelper is the child the test above starts, and skips unless run as that child.
func TestToolRunStopGraceHelper(t *testing.T) {
	args := flag.Args()
	if len(args) != 2 || (args[1] != "own" && args[1] != "same") {
		t.Skip("runs only as the child of TestToolRunStopGraceKillsWhatOutlivesTheCommand")
	}
	beat, group := args[0], args[1]
	commandPgid, err := syscall.Getpgid(os.Getppid())
	if err != nil {
		t.Fatal(err)
	}
	if group == "own" {
		if err := syscall.Setpgid(0, 0); err != nil {
			t.Fatal(err)
		}
	}
	signal.Ignore(syscall.SIGTERM)
	record := fmt.Sprintf("%d %d %d\n", os.Getpid(), syscall.Getpgrp(), commandPgid)
	if err := os.WriteFile(beat+".tmp", []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(beat, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(beat+".tmp", beat+".pid"); err != nil {
		t.Fatal(err)
	}
	for {
		f, err := os.OpenFile(beat, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString("x\n")
		if err = errors.Join(err, f.Close()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// gnuTimeout is GNU timeout's path, as timeout or as Homebrew's gtimeout.
// BusyBox's timeout, Alpine's, is passed over: it leaves the command in the
// caller's group, so the case would pass without the escape it exists for.
func gnuTimeout(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"timeout", "gtimeout"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if version, err := exec.CommandContext(t.Context(), path, "--version").Output(); err == nil && strings.Contains(string(version), "GNU coreutils") {
			return path
		}
	}
	t.Skip("no GNU timeout on PATH, as timeout or gtimeout")
	return ""
}

func TestToolRunPassesItsEnv(t *testing.T) {
	t.Log("Doing: a failing command that prints a variable only the run's env sets")
	dir := t.TempDir()
	run := &tenantToolRun{dir: dir, env: []string{"KB_OPENSEARCH_URL=https://opensearch.example.test", "KB_OPENSEARCH_PASS=opensearch-pass"}}
	err := run.try(t.Context(), dir, "sh", "-c", `echo "url=${KB_OPENSEARCH_URL}"; exit 1`)
	t.Log("Testing: the command sees the variable next to the environment try builds")
	require.ErrorContains(t, err, "url=https://opensearch.example.test")
	t.Log("Testing: the error, which the spec logs, names the command but not the run's env")
	require.NotContains(t, err.Error(), "opensearch-pass")
}

func TestAICRValidateResult(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	checks := write("checks.json", `{"results":{"tool":{"version":"1.2.0"},"tests":[{"name":"operator-health","status":"passed"}]}}`)
	empty := write("empty.json", `{"results":{"tests":[]}}`)
	missing := filepath.Join(dir, "missing.json")
	exitErr := errors.New("aicr validate: exit status 1")

	for _, tc := range []struct {
		name    string
		runErr  error
		report  string
		wantErr string
		summary string
	}{
		{name: "exit 0 with checks", report: checks, summary: "passed  operator-health"},
		// aicr exits 0 for a phase with no checks; that must not pass.
		{name: "exit 0 with an empty report", report: empty, wantErr: "lists no checks", summary: "no report"},
		{name: "exit 0 with no report", report: missing, wantErr: "at least one deployment check", summary: "no report"},
		{name: "a failed exit with checks", runErr: exitErr, report: checks, wantErr: "exit status 1", summary: "passed  operator-health"},
		{name: "a failed exit with no report", runErr: exitErr, report: missing, wantErr: "exit status 1", summary: "no report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary, err := aicrValidateResult(tc.runErr, tc.report)
			if !strings.Contains(summary, tc.summary) {
				t.Errorf("summary = %q, want it to contain %q", summary, tc.summary)
			}
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("err = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestTenantGPURuntimePlanSetsTheGPUCountOnlyWhenItInstalls(t *testing.T) {
	shape := clusterShape{workerPools: []nvke.NodePoolSpec{{DesiredNodeCount: 1}, {DesiredNodeCount: 2}}}

	settings := tenantGPURuntimePlan{accelerator: "gb200"}.tenantSettings(laneTenantSettings, shape, 5)
	if settings.gpuNodes == nil || *settings.gpuNodes != 3 {
		t.Fatalf("gpuNodes = %v, want every worker, 3", settings.gpuNodes)
	}
	if laneTenantSettings.gpuNodes != nil {
		t.Fatal("tenantSettings changed the shared lane settings")
	}

	// The Forge and DSX tenants have no worker pools in their shape; their
	// builder makes -e2e.tenant-workers workers.
	hardware := tenantGPURuntimePlan{accelerator: "gb200"}.tenantSettings(laneTenantSettings, nicoForgeTenant().shape, 2)
	if hardware.gpuNodes == nil || *hardware.gpuNodes != 2 {
		t.Fatalf("hardware tenant gpuNodes = %v, want -e2e.tenant-workers, 2", hardware.gpuNodes)
	}

	for _, plan := range []tenantGPURuntimePlan{{}, {skip: "no recipe"}} {
		if got := plan.tenantSettings(laneTenantSettings, shape, 5); got.gpuNodes != nil {
			t.Errorf("plan %+v set gpuNodes = %d; the GPU checks need a runtime to run against", plan, *got.gpuNodes)
		}
	}
}

// TestAICRRecipeLeavesTheFabricOut renders the recipe the install spec renders
// and checks that the overlay in aicrDataDir still lands on AICR's NVKE leaf.
// An aicr bump that renames the leaf or a fabric component fails here rather
// than on hardware. Rendering is offline; it needs only aicr on PATH.
func TestAICRRecipeLeavesTheFabricOut(t *testing.T) {
	if _, err := exec.LookPath("aicr"); err != nil {
		t.Skip("aicr is not on PATH")
	}
	output := filepath.Join(t.TempDir(), "recipe.yaml")
	command := exec.CommandContext(t.Context(), "aicr", aicrRecipeArgs("gb200", aicrDataDir, output)...)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("aicr recipe: %v\n%s", err, out)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var recipe struct {
		Metadata struct {
			AppliedOverlays []string `json:"appliedOverlays"`
		} `json:"metadata"`
		DeploymentOrder []string `json:"deploymentOrder"`
	}
	if err := yaml.Unmarshal(data, &recipe); err != nil {
		t.Fatal(err)
	}

	for _, overlay := range []string{"gb200-nvke-ubuntu-training", "gb200-nvke-ubuntu-training-no-fabric"} {
		if !slices.Contains(recipe.Metadata.AppliedOverlays, overlay) {
			t.Errorf("applied overlays %v do not include %s", recipe.Metadata.AppliedOverlays, overlay)
		}
	}
	for _, component := range []string{"network-operator", "sriov-ib-networks", "rdma-modules"} {
		if slices.Contains(recipe.DeploymentOrder, component) {
			t.Errorf("deployment order %v still installs %s", recipe.DeploymentOrder, component)
		}
	}
	if !slices.Contains(recipe.DeploymentOrder, "gpu-operator") {
		t.Errorf("deployment order %v does not install gpu-operator", recipe.DeploymentOrder)
	}
}

func TestSummarizeCTRF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	report := `{"reportFormat":"CTRF","results":{"tool":{"name":"aicr","version":"v1.1.2"},
		"summary":{"tests":2,"passed":1,"failed":1},
		"tests":[{"name":"operator-health","status":"passed","message":"ok"},
		         {"name":"check-nvidia-smi","status":"failed","message":"no GPU on node"}]}}`
	if err := os.WriteFile(path, []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := summarizeCTRF(path)
	if err != nil {
		t.Fatalf("summarizeCTRF() error = %v", err)
	}
	want := "aicr v1.1.2\npassed  operator-health\nfailed  check-nvidia-smi: no GPU on node"
	if got != want {
		t.Errorf("summarizeCTRF() =\n%s\nwant\n%s", got, want)
	}
}

func TestSummarizeCTRFRejectsAReportWithNoChecks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, []byte(`{"results":{"tests":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// An empty report would otherwise read as a run where nothing failed.
	if _, err := summarizeCTRF(path); err == nil || !strings.Contains(err.Error(), "lists no checks") {
		t.Fatalf("summarizeCTRF() error = %v, want one saying the report lists no checks", err)
	}
}

func TestAICRTokenWarning(t *testing.T) {
	deadline := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		expiresAt   time.Time
		known       bool
		hasDeadline bool
		want        string
	}{
		{name: "outlasts the spec", expiresAt: deadline.Add(5 * time.Minute), known: true, hasDeadline: true},
		{name: "expires at the deadline", expiresAt: deadline, known: true, hasDeadline: true},
		{
			name: "capped short", expiresAt: deadline.Add(-20 * time.Minute), known: true, hasDeadline: true,
			want: "expires at 2026-11-13T11:40:00Z, 20m0s before this spec's deadline",
		},
		{name: "expiry unknown", expiresAt: deadline.Add(-20 * time.Minute), hasDeadline: true},
		{name: "no deadline", expiresAt: deadline.Add(-20 * time.Minute), known: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := aicrTokenWarning(tc.expiresAt, tc.known, deadline, tc.hasDeadline)
			switch {
			case tc.want == "" && got != "":
				t.Errorf("aicrTokenWarning() = %q, want none", got)
			case tc.want != "" && !strings.Contains(got, tc.want):
				t.Errorf("aicrTokenWarning() = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

func TestTailBufferKeepsTheEndOfItsOutput(t *testing.T) {
	short := &tailBuffer{limit: 64}
	fmt.Fprint(short, "one\ntwo\n")
	if got := short.String(); got != "one\ntwo\n" {
		t.Errorf("under the limit: String() = %q, want everything written", got)
	}

	long := &tailBuffer{limit: 16}
	for i := range 10 {
		fmt.Fprintf(long, "line %d\n", i)
	}
	// The limit lands inside "line 7"; a cut first line is left out.
	if got := long.String(); got != "line 8\nline 9\n" {
		t.Errorf("over the limit: String() = %q, want the whole lines at the end", got)
	}
	if len(long.buf) > long.limit {
		t.Errorf("kept %d bytes, want at most %d", len(long.buf), long.limit)
	}
}

func TestLastLines(t *testing.T) {
	if got := lastLines("a\nb\nc\n", 2); got != "b\nc" {
		t.Errorf("lastLines(3 lines, 2) = %q, want %q", got, "b\nc")
	}
	if got := lastLines("a\nb", 5); got != "a\nb" {
		t.Errorf("lastLines(2 lines, 5) = %q, want %q", got, "a\nb")
	}
}
