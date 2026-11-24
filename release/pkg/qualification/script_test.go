// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// These test release/hack/qualify-e2e.sh, the steps the qualification
// workflow and `qualify local` both run. Only the steps that reach nothing
// past git are run here: the ones that pull, build or take a machine are the
// workflow's to prove.
package qualification_test

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const script = "../../hack/qualify-e2e.sh"

// shells are the bashes to run the script under: whatever is on PATH, and
// /bin/bash, which on macOS is 3.2 and is what a workstation runs it with.
func shells(t *testing.T) []string {
	t.Helper()
	var out []string
	seen := map[string]bool{}
	for _, name := range []string{"bash", "/bin/bash"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || seen[resolved] {
			continue
		}
		seen[resolved] = true
		out = append(out, path)
	}
	if len(out) == 0 {
		t.Fatal("no bash to run the script with")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not on PATH; the script needs it")
	}
	return out
}

func git(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", args...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// plan is the fixture with its commit set to this checkout's HEAD, which is
// a commit the resolve step can find without fetching.
func plan(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/script-plan.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(b), "COMMIT", git(t, "rev-parse", "HEAD"))), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// steps runs the script's steps in order with what the earlier ones wrote, as
// the workflow does through $GITHUB_ENV.
type steps struct {
	t     *testing.T
	shell string
	env   map[string]string
	file  string
}

func newSteps(t *testing.T, shell string, env map[string]string) *steps {
	t.Helper()
	l := &steps{t: t, shell: shell, file: filepath.Join(t.TempDir(), "steps.env"), env: map[string]string{
		"QUALIFY_E2E_SITE_CONTROLLER_VERSION": "",
		"SITE_STAGE":                          "full",
		"DRY_RUN":                             "true",
		"SITE_IMAGE_REGISTRY":                 "nvcr",
		"QUALIFY_E2E_NGC_ORG":                 "example-org",
		"NGC_API_KEY":                         "key",
		"SITE_INPUTS":                         "/w/site-inputs",
	}}
	for k, v := range env {
		l.env[k] = v
	}
	if err := os.WriteFile(l.file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return l
}

func (l *steps) values() map[string]string {
	l.t.Helper()
	f, err := os.Open(l.file)
	if err != nil {
		l.t.Fatal(err)
	}
	defer f.Close()
	out := map[string]string{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		if k, v, ok := strings.Cut(s.Text(), "="); ok {
			out[k] = v
		}
	}
	return out
}

func (l *steps) set(k, v string) {
	l.t.Helper()
	f, err := os.OpenFile(l.file, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		l.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(k + "=" + v + "\n"); err != nil {
		l.t.Fatal(err)
	}
}

// step runs one step and returns its error output, failing the test when
// the step's outcome is not the one wanted.
func (l *steps) step(wantOK bool, args ...string) string {
	l.t.Helper()
	cmd := exec.CommandContext(l.t.Context(), l.shell, append([]string{script}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "STEP_ENV=" + l.file}
	for k, v := range l.env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	for k, v := range l.values() {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if (err == nil) != wantOK {
		l.t.Fatalf("%s %v: err = %v, want ok = %t\n%s", l.shell, args, err, wantOK, out)
	}
	return string(out)
}

func expect(t *testing.T, got, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestStreamEnvReadsOneStreamFromThePlan(t *testing.T) {
	p := plan(t)
	for _, sh := range shells(t) {
		l := newSteps(t, sh, nil)
		l.step(true, "stream-env", p, "", "forge-l40s-amd64")
		expect(t, l.values(), map[string]string{
			"COMBINATION":                      "combo-1",
			"SITE_NVKE_CONTROLLER_VERSION":     "v0.2.0-3-gabcdef12",
			"NVKE_CONTROLLER_VERSION_STRATEGY": "commitDescribe",
			"CLI_VERSION":                      "v0.3.0-3-gabcdef12",
			"CLI_REF":                          "oci://registry.example/org/nvkectl:v0.3.0-3-gabcdef12-linux-amd64",
			"KUBERNETES_VERSION":               "v1.36.0",
			"OS_IMAGE_AMD64":                   "https://cdn.example/image-amd64.qcow2",
			"OS_IMAGE_BOOTLOADER_ARM64":        "https://cdn.example/bootloader-arm64.raw",
			"ARCH":                             "amd64",
			"PROVIDER":                         "forge",
			"GPU":                              "l40s",
			"UBUNTU":                           "24.04",
			"PLAN_COMMIT":                      git(t, "rev-parse", "HEAD"),
		})
	}
}

// A stream's environment can install another controller than the plan names.
func TestStreamEnvTakesTheSiteControllerOverride(t *testing.T) {
	p := plan(t)
	for _, sh := range shells(t) {
		l := newSteps(t, sh, map[string]string{"QUALIFY_E2E_SITE_CONTROLLER_VERSION": "v9.9.9"})
		l.step(true, "stream-env", p, "", "forge-l40s-amd64")
		expect(t, l.values(), map[string]string{"SITE_NVKE_CONTROLLER_VERSION": "v9.9.9"})
	}
}

func TestStreamEnvRefusesAStreamThePlanLacks(t *testing.T) {
	p := plan(t)
	for _, sh := range shells(t) {
		if out := newSteps(t, sh, nil).step(false, "stream-env", p, "", "nope"); !strings.Contains(out, "no stream nope") {
			t.Errorf("output = %q, want it to name the stream", out)
		}
	}
}

// TestMatrixHasOneEntryPerStream checks that the matrix has an entry for every
// stream in the plan.
func TestMatrixHasOneEntryPerStream(t *testing.T) {
	p := plan(t)
	for _, sh := range shells(t) {
		out, err := exec.CommandContext(t.Context(), sh, script, "matrix", p).Output()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(string(out), `"stream":`); got != 3 {
			t.Errorf("matrix has %d entries, want 3: %s", got, out)
		}
	}
}

// A stream's parameters reach the steps as the variables they read, and a
// stream that names none leaves them empty.
func TestStreamEnvEmitsTheStreamsParameters(t *testing.T) {
	p := plan(t)
	for _, sh := range shells(t) {
		l := newSteps(t, sh, nil)
		l.step(true, "stream-env", p, "", "forge-cpu-amd64-perf")
		expect(t, l.values(), map[string]string{
			"E2E_ENVIRONMENT":          "forge-perf",
			"E2E_TENANT_WORKERS":       "3",
			"E2E_MONITORING_WORKERS":   "1",
			"SUITE_TIMEOUT_MINUTES":    "215",
			"E2E_KAPS_BENCH_RUN_SPECS": "smoke/smoke.yaml,netperf/hostnet-stream.yaml",
		})

		l = newSteps(t, sh, nil)
		l.step(true, "stream-env", p, "", "forge-l40s-amd64")
		expect(t, l.values(), map[string]string{
			"E2E_ENVIRONMENT":          "",
			"E2E_TENANT_WORKERS":       "",
			"E2E_MONITORING_WORKERS":   "",
			"SUITE_TIMEOUT_MINUTES":    "",
			"E2E_KAPS_BENCH_RUN_SPECS": "",
		})
	}
}

// The job's timeout follows from the suite's: the default suite timeout gives
// the job timeout the workflow always had, and the GPU runtime adds its 65
// minutes to either.
func TestMatrixDerivesTheJobTimeout(t *testing.T) {
	p := plan(t)
	for _, sh := range shells(t) {
		out, err := exec.CommandContext(t.Context(), sh, script, "matrix", p).Output()
		if err != nil {
			t.Fatal(err)
		}
		var matrix struct {
			Include []struct {
				Stream                      string `json:"stream"`
				Environment                 string `json:"environment"`
				TenantWorkers               string `json:"tenant_workers"`
				MonitoringWorkers           string `json:"monitoring_workers"`
				JobTimeoutMinutes           int    `json:"job_timeout_minutes"`
				JobTimeoutMinutesGPURuntime int    `json:"job_timeout_minutes_gpu_runtime"`
			} `json:"include"`
		}
		if err := json.Unmarshal(out, &matrix); err != nil {
			t.Fatalf("matrix is not JSON: %v\n%s", err, out)
		}
		type entry struct {
			environment, workers, monitoring string
			timeout, gpuRuntime              int
		}
		want := map[string]entry{
			"forge-l40s-amd64":     {"", "", "", 335, 400},
			"dsx-cpu-arm64":        {"", "", "", 335, 400},
			"forge-cpu-amd64-perf": {"forge-perf", "3", "1", 400, 465},
		}
		for _, e := range matrix.Include {
			got := entry{e.Environment, e.TenantWorkers, e.MonitoringWorkers, e.JobTimeoutMinutes, e.JobTimeoutMinutesGPURuntime}
			if got != want[e.Stream] {
				t.Errorf("%s: %s matrix entry = %+v, want %+v", sh, e.Stream, got, want[e.Stream])
			}
		}
	}
}

func TestResolveAPublishedStream(t *testing.T) {
	p := plan(t)
	for _, sh := range shells(t) {
		l := newSteps(t, sh, nil)
		l.step(true, "stream-env", p, "", "forge-l40s-amd64")
		l.step(true, "resolve")
		expect(t, l.values(), map[string]string{
			"SITE_NVKE_CONTROLLER_BUILD": "0",
			"SITE_PROVISIONED_BUILD":     "0",
			"SITE_IMAGES":                "nvcr.io/example-org",
			"SITE_ARCH":                  "amd64",
			"SITE_ENVIRONMENT":           "forge",
			"SITE_TENANT_EXCLUDE":        "nico-dsx",
			"CAPABILITIES":               "arch=amd64,gpu=l40s,ubuntu=24.04",
			"SITE_CLI_BUILD":             "0",
			"SITE_CLI_REF":               "registry.example/org/nvkectl:v0.3.0-3-gabcdef12-linux-amd64",
			"SITE_NVKECTL":               "/w/site-inputs/nvkectl/nvkectl-linux-amd64",
			"OS_IMAGE_VERSIONS": "os-image-bootloader-amd64=https://cdn.example/bootloader-amd64.raw," +
				"os-image-amd64=https://cdn.example/image-amd64.qcow2," +
				"os-image-bootloader-arm64=https://cdn.example/bootloader-arm64.raw," +
				"os-image-arm64=https://cdn.example/image-arm64.qcow2," +
				"kube=v1.36.0,cni-ovn=1.2.3,kind=v0.30.0,clusterctl=v1.11.0,nerdctl=2.1.0",
		})
	}
}

// An empty version is how a run asks for a component to be built from the
// checkout, named by its commit.
func TestResolveBuildsWhatNamesNoVersion(t *testing.T) {
	p := plan(t)
	head := git(t, "rev-parse", "--short=12", "HEAD")
	for _, sh := range shells(t) {
		l := newSteps(t, sh, map[string]string{"SITE_IMAGE_REGISTRY": "ghcr"})
		l.step(true, "stream-env", p, "", "forge-l40s-amd64")
		l.set("SITE_NVKE_CONTROLLER_VERSION", "")
		l.set("CLI_VERSION", "")
		l.set("CAPI_PROVISIONED_VERSION", "")
		l.step(true, "resolve")
		expect(t, l.values(), map[string]string{
			"SITE_NVKE_CONTROLLER_BUILD":   "1",
			"SITE_NVKE_CONTROLLER_VERSION": head,
			"SITE_PROVISIONED_BUILD":       "1",
			"CAPI_PROVISIONED_VERSION":     head,
			"SITE_CLI_BUILD":               "1",
			"CLI_VERSION":                  head,
			"SITE_NVKECTL":                 "cli/dist/nvkectl-linux-amd64",
			"SITE_IMAGES":                  "ghcr.io/dsx-ai-factory/nvke",
		})
	}
}

// Every image built here: nothing is pulled, so no registry credential is
// read, and the images are named under a prefix no registry answers to.
func TestResolveLocalNeedsNoRegistry(t *testing.T) {
	p := plan(t)
	for _, sh := range shells(t) {
		l := newSteps(t, sh, map[string]string{"SITE_IMAGE_REGISTRY": "local", "QUALIFY_E2E_NGC_ORG": "", "NGC_API_KEY": ""})
		l.step(true, "stream-env", p, "", "forge-l40s-amd64")
		l.set("SITE_NVKE_CONTROLLER_VERSION", "")
		l.set("CLI_VERSION", "")
		l.set("CAPI_PROVISIONED_VERSION", "")
		l.step(true, "resolve")
		expect(t, l.values(), map[string]string{"SITE_IMAGES": "localhost/nvke"})
	}
}

func TestResolveBuildsTheControllerForArm64(t *testing.T) {
	p := plan(t)
	for _, sh := range shells(t) {
		l := newSteps(t, sh, nil)
		l.step(true, "stream-env", p, "", "dsx-cpu-arm64")
		l.set("OS_IMAGE_AMD64", "https://cdn.example/image-amd64.qcow2")
		l.set("SITE_NVKE_CONTROLLER_VERSION", "")
		l.step(true, "resolve")
		expect(t, l.values(), map[string]string{"SITE_NVKE_CONTROLLER_BUILD": "1", "SITE_ARCH": "arm64"})
	}
}

func TestResolveMapsTheDSXProvider(t *testing.T) {
	p := plan(t)
	for _, sh := range shells(t) {
		l := newSteps(t, sh, nil)
		l.step(true, "stream-env", p, "", "dsx-cpu-arm64")
		l.set("OS_IMAGE_AMD64", "https://cdn.example/image-amd64.qcow2")
		l.step(true, "resolve")
		expect(t, l.values(), map[string]string{
			"SITE_ENVIRONMENT":    "dsx",
			"SITE_TENANT_EXCLUDE": "nico-forge",
			"CAPABILITIES":        "arch=arm64",
		})
	}
}

// TestResolveTakesTheStreamsEnvironment checks that a stream naming an
// environment resolves to it, with its provider's tenant exclusion.
func TestResolveTakesTheStreamsEnvironment(t *testing.T) {
	p := plan(t)
	for _, sh := range shells(t) {
		l := newSteps(t, sh, nil)
		l.step(true, "stream-env", p, "", "forge-cpu-amd64-perf")
		l.step(true, "resolve")
		expect(t, l.values(), map[string]string{
			"SITE_ENVIRONMENT":    "forge-perf",
			"SITE_TENANT_EXCLUDE": "nico-dsx",
			"CAPABILITIES":        "arch=amd64",
		})
	}
}

// An environment a stream names has to be one the suite has, for the stream's
// provider.
func TestResolveRefusesAnEnvironmentThatIsNotTheStreams(t *testing.T) {
	p := plan(t)
	for _, tc := range []struct{ name, environment, want string }{
		{"a file that does not exist", "forge-nope", "does not exist"},
		{"another provider's environment", "dsx", "does not declare provider: forge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, sh := range shells(t) {
				l := newSteps(t, sh, nil)
				l.step(true, "stream-env", p, "", "forge-l40s-amd64")
				l.set("E2E_ENVIRONMENT", tc.environment)
				if out := l.step(false, "resolve"); !strings.Contains(out, tc.want) {
					t.Errorf("output = %q, want %q in it", out, tc.want)
				}
			}
		})
	}
}

func TestResolveRefuses(t *testing.T) {
	p := plan(t)
	for _, tc := range []struct {
		name, stream string
		env          map[string]string
		after        map[string]string
		want         string
	}{
		{"an image without its bootloader", "dsx-cpu-arm64", nil, nil, "go together"},
		{"nvcr without an NGC key", "forge-l40s-amd64", map[string]string{"NGC_API_KEY": ""}, nil, "NGC_API_KEY"},
		{"an unknown registry", "forge-l40s-amd64", map[string]string{"SITE_IMAGE_REGISTRY": "quay"}, nil, "is not ghcr, nvcr or local"},
		{
			"local with an image to pull", "forge-l40s-amd64",
			map[string]string{"SITE_IMAGE_REGISTRY": "local"},
			map[string]string{"SITE_NVKE_CONTROLLER_VERSION": "", "CLI_VERSION": ""},
			"capi-provisioned would be pulled",
		},
		{
			"a workflow plan naming no capi-provisioned", "forge-l40s-amd64",
			map[string]string{"GITHUB_ACTIONS": "true"},
			map[string]string{"CAPI_PROVISIONED_VERSION": ""},
			"the plan names no capi-provisioned version",
		},
		{"a value with a line break", "forge-l40s-amd64", map[string]string{"QUALIFY_E2E_NGC_ORG": "example-org\nBASH_ENV=/tmp/x"}, nil, "has a line break in its value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, sh := range shells(t) {
				l := newSteps(t, sh, tc.env)
				l.step(true, "stream-env", p, "", tc.stream)
				for k, v := range tc.after {
					l.set(k, v)
				}
				if out := l.step(false, "resolve"); !strings.Contains(out, tc.want) {
					t.Errorf("output = %q, want %q in it", out, tc.want)
				}
			}
		})
	}
}

// A caller that does not set a name a step reads is told which, rather than
// the step running on an empty value it did not choose.
func TestAStepRefusesAnUnsetInput(t *testing.T) {
	p := plan(t)
	for _, sh := range shells(t) {
		l := newSteps(t, sh, nil)
		l.step(true, "stream-env", p, "", "forge-l40s-amd64")
		delete(l.env, "SITE_INPUTS")
		if out := l.step(false, "resolve"); !strings.Contains(out, "resolve reads SITE_INPUTS") {
			t.Errorf("output = %q, want it to name SITE_INPUTS", out)
		}
	}
}

func TestConfigKeepsWhatItIsAskedTo(t *testing.T) {
	for _, tc := range []struct {
		site, tenant, keyDir string
		want                 string
	}{
		{"false", "false", "", ""},
		{"true", "false", "", "lifecycle:\n  preserveOnFailure: true\n"},
		{"false", "true", "", "lifecycle:\n  skipDelete: true\n"},
		{"true", "true", "", "lifecycle:\n  skipDelete: true\n"},
		{"false", "true", "/r/node-keys", "lifecycle:\n  skipDelete: true\n  nodeKeyDir: '/r/node-keys'\n"},
		{"false", "false", "/r/node-keys", "lifecycle:\n  nodeKeyDir: '/r/node-keys'\n"},
		// Unquoted, YAML would read " #1..." as a comment.
		{"false", "false", "/r/run #1/ron's/node-keys", "lifecycle:\n  nodeKeyDir: '/r/run #1/ron''s/node-keys'\n"},
	} {
		for _, sh := range shells(t) {
			config := filepath.Join(t.TempDir(), "e2e-config.yaml")
			l := newSteps(t, sh, map[string]string{
				"E2E_CONFIG":                      config,
				"NODE_USER":                       "",
				"SITE_NVKECTL":                    "/w/nvkectl",
				"SITE_DEPLOY_PATH_REF":            "",
				"SITE_IMAGES":                     "localhost/nvke",
				"SITE_NVKE_CONTROLLER_REPOSITORY": "nvke-controller",
				"KEEP_SITE":                       tc.site,
				"KEEP_TENANT":                     tc.tenant,
				"NODE_KEY_DIR":                    tc.keyDir,
			})
			l.step(true, "config")
			b, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			_, got, _ := strings.Cut(string(b), "lifecycle:\n")
			if got != "" {
				got = "lifecycle:\n" + got
			}
			if got != tc.want {
				t.Errorf("%s keep-site=%s keep-tenant=%s node-key-dir=%q: lifecycle = %q, want %q", sh, tc.site, tc.tenant, tc.keyDir, got, tc.want)
			}
		}
	}
}

// The key warning says a key is written, so it is given only when one will be:
// a dry run takes no machine.
func TestConfigWarnsOfTheNodeKeyOnlyWhenOneIsWritten(t *testing.T) {
	for _, dryRun := range []string{"true", "false"} {
		for _, sh := range shells(t) {
			l := newSteps(t, sh, map[string]string{
				"E2E_CONFIG":                      filepath.Join(t.TempDir(), "e2e-config.yaml"),
				"NODE_USER":                       "",
				"SITE_NVKECTL":                    "/w/nvkectl",
				"SITE_DEPLOY_PATH_REF":            "",
				"SITE_IMAGES":                     "localhost/nvke",
				"SITE_NVKE_CONTROLLER_REPOSITORY": "nvke-controller",
				"KEEP_SITE":                       "true",
				"KEEP_TENANT":                     "false",
				"NODE_KEY_DIR":                    "/r/node-keys",
				"DRY_RUN":                         dryRun,
			})
			out := l.step(true, "config")
			if warned := strings.Contains(out, "SSH key is written"); warned == (dryRun == "true") {
				t.Errorf("%s dry-run=%s: warned = %t\n%s", sh, dryRun, warned, out)
			}
		}
	}
}

// copiedScript is the script in a copy of the repository's layout, because the
// run step clears and writes the suite's outputs beside it.
func copiedScript(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	copied := filepath.Join(root, "release", "hack", "qualify-e2e.sh")
	for _, dir := range []string{filepath.Dir(copied), filepath.Join(root, "controller", "test", "e2e")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(copied, b, 0o700); err != nil {
		t.Fatal(err)
	}
	return copied
}

// dryRun runs the run step as a dry run from the copy, with the values a run
// step reads, the two a stream's parameters set, and the GPU runtime switch.
func dryRun(t *testing.T, sh, copied, workers, suite, gpuRuntime string, extra ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), sh, copied, "run")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"),
		"STEP_ENV=" + filepath.Join(t.TempDir(), "steps.env"),
		"DRY_RUN=true", "KEEP_SITE=false", "KEEP_TENANT=false",
		"SITE_TENANT_EXCLUDE=nico-dsx", "SITE_STAGE=full", "SITE_ENVIRONMENT=forge-perf",
		"SITE_NVKE_CONTROLLER_VERSION=v0.3.0", "CLI_VERSION=v0.3.0", "CAPI_PROVISIONED_VERSION=v0.3.0",
		"OS_IMAGE_VERSIONS=os-image-amd64=https://cdn.example/image-amd64.qcow2", "CAPABILITIES=arch=amd64",
		"NVKE_CONTROLLER_NAMESPACE=", "NVKE_CONTROLLER_SERVICE=",
		"E2E_TENANT_WORKERS=" + workers, "E2E_MONITORING_WORKERS=", "SUITE_TIMEOUT_MINUTES=" + suite,
		"TENANT_GPU_RUNTIME=" + gpuRuntime,
		"E2E_KAPS_BENCH_RUN_SPECS=", "E2E_KAPS_BENCH_DIR=",
	}
	cmd.Env = append(cmd.Env, extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestRunDerivesTheTimeoutsAndPassesTheWorkers checks the suite arguments of a
// dry run: the timeouts follow the suite timeout, the GPU runtime moves them out
// by its 65 minutes, and the tenant workers, monitoring workers and GPU runtime
// flags are passed only when they are set.
func TestRunDerivesTheTimeoutsAndPassesTheWorkers(t *testing.T) {
	copied := copiedScript(t)
	for _, tc := range []struct {
		name, workers, monitoring, suite, gpuRuntime string
		want                                         []string
		notWant                                      string
	}{
		{"a stream naming nothing", "", "", "", "false", []string{"-ginkgo.timeout=150m -timeout 220m"}, "-e2e.tenant"},
		{"a perf stream", "3", "", "215", "false", []string{"-ginkgo.timeout=215m -timeout 285m", "  -e2e.tenant-workers=3\n"}, "-e2e.tenant-gpu-runtime"},
		{"a perf stream with a monitoring node", "3", "1", "215", "false", []string{"  -e2e.tenant-workers=3 -e2e.monitoring-workers=1\n"}, ""},
		{"the GPU runtime", "", "", "", "true", []string{"-ginkgo.timeout=215m -timeout 285m", "  -e2e.tenant-gpu-runtime\n"}, ""},
		{"the GPU runtime on a perf stream", "", "", "215", "true", []string{"-ginkgo.timeout=280m -timeout 350m"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, sh := range shells(t) {
				out, err := dryRun(t, sh, copied, tc.workers, tc.suite, tc.gpuRuntime, "E2E_MONITORING_WORKERS="+tc.monitoring)
				if err != nil {
					t.Fatalf("%s run: %v\n%s", sh, err, out)
				}
				for _, want := range tc.want {
					if !strings.Contains(out, want) {
						t.Errorf("%s run: output = %q, want %q in it", sh, out, want)
					}
				}
				if tc.notWant != "" && strings.Contains(out, tc.notWant) {
					t.Errorf("%s run: output = %q, want no %q in it", sh, out, tc.notWant)
				}
			}
		})
	}
}

// TestRunMasksTheHostOfAWithheldURL checks that on Actions the run step masks
// a DSX API host alone, which GitHub's masking of the secret does not cover,
// and masks no fragment of the OpenSearch password.
func TestRunMasksTheHostOfAWithheldURL(t *testing.T) {
	copied := copiedScript(t)
	for _, tc := range []struct{ api, host string }{
		{"https://api.hall3.example:443/", "api.hall3.example"},
		{" https://api.hall3.example:443 ", "api.hall3.example"},
		{"\nhttps://api.hall3.example:443\n", "api.hall3.example"},
		{"https://[fd00::3]:443/", "fd00::3"},
	} {
		for _, sh := range shells(t) {
			out, err := dryRun(t, sh, copied, "", "", "false", "GITHUB_ACTIONS=true", "DSX_DATAHALL_3_API="+tc.api)
			if err != nil {
				t.Fatalf("%s run: %v\n%s", sh, err, out)
			}
			if !strings.Contains(out, "::add-mask::"+tc.host+"\n") {
				t.Errorf("%s run with %q: output = %q, want %s masked", sh, tc.api, out, tc.host)
			}
		}
	}
	for _, sh := range shells(t) {
		out, err := dryRun(t, sh, copied, "", "", "false", "GITHUB_ACTIONS=true",
			"KB_OPENSEARCH_URL=https://opensearch.example:9200", "KB_OPENSEARCH_PASS=p/ass")
		if err != nil {
			t.Fatalf("%s run: %v\n%s", sh, err, out)
		}
		if !strings.Contains(out, "::add-mask::opensearch.example\n") {
			t.Errorf("%s run: output = %q, want opensearch.example masked", sh, out)
		}
		if strings.Contains(out, "::add-mask::p\n") {
			t.Errorf("%s run: output = %q, want no fragment of the password masked", sh, out)
		}
	}
}

// TestRunPassesTheKapsBenchSettings checks that a dry run shows the kaps-bench
// flags only when they are set, and leaves the timeouts as the suite's.
func TestRunPassesTheKapsBenchSettings(t *testing.T) {
	copied := copiedScript(t)
	for _, tc := range []struct {
		name    string
		extra   []string
		want    []string
		notWant string
	}{
		{"a stream running no kaps-bench", nil, nil, "-e2e.kaps-bench"},
		{
			"a stream running two run specs",
			[]string{"E2E_KAPS_BENCH_RUN_SPECS=smoke/smoke.yaml,netperf/hostnet-stream.yaml", "E2E_KAPS_BENCH_DIR=/opt/kaps-bench"},
			[]string{"-e2e.kaps-bench-run-specs=smoke/smoke.yaml,netperf/hostnet-stream.yaml", "-e2e.kaps-bench-dir=/opt/kaps-bench", "-ginkgo.timeout=150m -timeout 220m"},
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, sh := range shells(t) {
				out, err := dryRun(t, sh, copied, "", "", "false", tc.extra...)
				if err != nil {
					t.Fatalf("%s run: %v\n%s", sh, err, out)
				}
				for _, want := range tc.want {
					if !strings.Contains(out, want) {
						t.Errorf("%s run: output = %q, want %q in it", sh, out, want)
					}
				}
				if tc.notWant != "" && strings.Contains(out, tc.notWant) {
					t.Errorf("%s run: output = %q, want no %q in it", sh, out, tc.notWant)
				}
			}
		})
	}
}

// TestRunPassesTheStreamSettingsToGoTest checks that a run hands go test the
// flags and timeout the dry run shows, through a go on PATH that records them.
func TestRunPassesTheStreamSettingsToGoTest(t *testing.T) {
	copied := copiedScript(t)
	bin := t.TempDir()
	argv := filepath.Join(t.TempDir(), "argv")
	fake := "#!/bin/sh\nprintf '%s\\n' \"$@\" >" + argv + "\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, sh := range shells(t) {
		out, err := dryRun(t, sh, copied, "3", "", "true", "DRY_RUN=false", "PATH="+bin+":"+os.Getenv("PATH"),
			"E2E_MONITORING_WORKERS=1", "E2E_KAPS_BENCH_RUN_SPECS=smoke/smoke.yaml", "E2E_KAPS_BENCH_DIR=/opt/kaps-bench")
		if err != nil {
			t.Fatalf("%s run: %v\n%s", sh, err, out)
		}
		b, err := os.ReadFile(argv)
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Split(string(b), "\n")
		for _, want := range []string{
			"-e2e.tenant-workers=3", "-e2e.monitoring-workers=1", "-e2e.tenant-gpu-runtime", "-ginkgo.timeout=215m",
			"-e2e.kaps-bench-run-specs=smoke/smoke.yaml", "-e2e.kaps-bench-dir=/opt/kaps-bench",
		} {
			if !slices.Contains(args, want) {
				t.Errorf("%s run: go test got %q, want %q among them", sh, args, want)
			}
		}
	}
}

// TestArtifactsCollectKapsBenchResults checks that the artifacts step lists
// kaps-bench's results and that a run removes a stale archive first.
func TestArtifactsCollectKapsBenchResults(t *testing.T) {
	copied := copiedScript(t)
	results := filepath.Join(filepath.Dir(copied), "..", "..", "controller", "test", "e2e", "kaps-bench-results.tar.gz")
	for _, sh := range shells(t) {
		if err := os.WriteFile(results, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.CommandContext(t.Context(), sh, copied, "artifacts").CombinedOutput()
		if err != nil || !strings.Contains(string(out), "/controller/test/e2e/kaps-bench-results.tar.gz\n") {
			t.Errorf("%s artifacts: err = %v, output = %q, want kaps-bench-results.tar.gz listed", sh, err, out)
		}

		if out, err := dryRun(t, sh, copied, "", "", "false"); err != nil {
			t.Fatalf("%s run: %v\n%s", sh, err, out)
		}
		if _, err := os.Stat(results); !os.IsNotExist(err) {
			t.Errorf("%s run: stale kaps-bench-results.tar.gz is still there: %v", sh, err)
		}
	}
}

// TestRunRefusesASuiteTimeoutThatIsNotANumber checks that a suite timeout that
// is not a number of minutes stops the run step.
func TestRunRefusesASuiteTimeoutThatIsNotANumber(t *testing.T) {
	copied := copiedScript(t)
	for _, sh := range shells(t) {
		out, err := dryRun(t, sh, copied, "", "abc", "false")
		if err == nil || !strings.Contains(out, "not a number of minutes") {
			t.Errorf("%s run: err = %v, output = %q, want it to refuse the timeout", sh, err, out)
		}
	}
}

// bash reads a script as it runs it. A step must not read past its own
// dispatch, or a run that outlives an edit to the script resumes in the
// edited text: here, a line appended after the dispatch.
func TestAStepReadsNothingPastTheDispatch(t *testing.T) {
	p := plan(t)
	b, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(t.TempDir(), "release", "hack", "qualify-e2e.sh")
	if err := os.MkdirAll(filepath.Dir(copied), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copied, append(b, []byte("\nnot-a-step-command\n")...), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, sh := range shells(t) {
		out, err := exec.CommandContext(t.Context(), sh, copied, "matrix", p).CombinedOutput()
		if err != nil || strings.Contains(string(out), "not-a-step-command") {
			t.Errorf("%s matrix ran past the dispatch: err = %v\n%s", sh, err, out)
		}
	}
}

func TestWithholdScrubsJumpHostsAndTheIssuer(t *testing.T) {
	for _, sh := range shells(t) {
		dir := t.TempDir()
		text := "through jump-1.example:22 and jump-2.example\nissuer https://issuer.example/ and https://issuer.example\nkept\n"
		for _, f := range []string{"qualify-e2e.log", "qualify-e2e.report.json", "site-cluster-state.txt"} {
			if err := os.WriteFile(filepath.Join(dir, f), []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		l := newSteps(t, sh, map[string]string{
			"FORGE_JUMP_HOST":        "jump-1.example",
			"FORGE_SITE_2_JUMP_HOST": "jump-2.example",
			"SSA_ISSUER":             "https://issuer.example/",
		})
		l.step(true, "withhold", dir)
		want := "through [withheld]:22 and [withheld]\nissuer [withheld] and [withheld]\nkept\n"
		for _, f := range []string{"qualify-e2e.log", "qualify-e2e.report.json", "site-cluster-state.txt"} {
			got, err := os.ReadFile(filepath.Join(dir, f))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != want {
				t.Errorf("%s: %s scrubbed to %q, want %q", sh, f, got, want)
			}
		}
	}
}

func TestWithholdScrubsTheDSXAPIHosts(t *testing.T) {
	for _, sh := range shells(t) {
		dir := t.TempDir()
		text := "Post \"https://api.hall3.example/realms/r/token\": dial tcp: lookup api.hall3.example: no such host\nsite hall3-site kept\n"
		if err := os.WriteFile(filepath.Join(dir, "qualify-e2e.log"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		l := newSteps(t, sh, map[string]string{
			"DSX_DATAHALL_3_API":     "https://api.hall3.example",
			"DSX_DATAHALL_3_SITE_ID": "hall3-site",
		})
		l.step(true, "withhold", dir)
		want := "Post \"[withheld]/realms/r/token\": dial tcp: lookup [withheld]: no such host\nsite hall3-site kept\n"
		got, err := os.ReadFile(filepath.Join(dir, "qualify-e2e.log"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s: scrubbed to %q, want %q", sh, got, want)
		}
	}
}

func TestWithholdScrubsTheOpenSearchEndpoint(t *testing.T) {
	for _, sh := range shells(t) {
		dir := t.TempDir()
		text := "ping opensearch: Get \"https://opensearch.example:9200\": dial tcp: lookup opensearch.example: no such host\nuser kaps kept\n"
		if err := os.WriteFile(filepath.Join(dir, "qualify-e2e.log"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		l := newSteps(t, sh, map[string]string{
			"KB_OPENSEARCH_URL":  "https://opensearch.example:9200",
			"KB_OPENSEARCH_USER": "kaps",
		})
		l.step(true, "withhold", dir)
		want := "ping opensearch: Get \"[withheld]\": dial tcp: lookup [withheld]: no such host\nuser kaps kept\n"
		got, err := os.ReadFile(filepath.Join(dir, "qualify-e2e.log"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s: scrubbed to %q, want %q", sh, got, want)
		}
	}
}

// A password is scrubbed as the whole value, and text matching only its host or trimmed form stays.
func TestWithholdScrubsTheOpenSearchPassword(t *testing.T) {
	for _, tc := range []struct{ pass, kept string }{
		{`p/a&s|s\w.o*r[d]^$`, "p kept"},
		{"x@y", "y"},
		{"pw:1234", "pw"},
	} {
		for _, sh := range shells(t) {
			dir := t.TempDir()
			text := "basic auth " + tc.pass + " refused\n" + tc.kept + "\n"
			if err := os.WriteFile(filepath.Join(dir, "qualify-e2e.log"), []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			l := newSteps(t, sh, map[string]string{"KB_OPENSEARCH_PASS": tc.pass})
			l.step(true, "withhold", dir)
			want := "basic auth [withheld] refused\n" + tc.kept + "\n"
			got, err := os.ReadFile(filepath.Join(dir, "qualify-e2e.log"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != want {
				t.Errorf("%s with %q: scrubbed to %q, want %q", sh, tc.pass, got, want)
			}
		}
	}
}

// A password holding a newline is scrubbed line by line, and the values withheld after it still are.
func TestWithholdScrubsThePasswordLineByLine(t *testing.T) {
	for _, tc := range []struct{ name, pass, text, want string }{
		{"a trailing newline", "s3cr3t-value\n", "basic auth s3cr3t-value refused\n", "basic auth [withheld] refused\n"},
		{"a newline in the middle", "first-half\nsecond-half", "basic auth first-half\nsecond-half refused\n", "basic auth [withheld]\n[withheld] refused\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, sh := range shells(t) {
				dir := t.TempDir()
				text := tc.text + "ping Get \"https://opensearch.example:9200\": lookup opensearch.example: no such host\nissuer https://issuer.example/\n"
				if err := os.WriteFile(filepath.Join(dir, "qualify-e2e.log"), []byte(text), 0o600); err != nil {
					t.Fatal(err)
				}
				l := newSteps(t, sh, map[string]string{
					"KB_OPENSEARCH_PASS": tc.pass,
					"KB_OPENSEARCH_URL":  "https://opensearch.example:9200",
					"SSA_ISSUER":         "https://issuer.example/",
				})
				l.step(true, "withhold", dir)
				want := tc.want + "ping Get \"[withheld]\": lookup [withheld]: no such host\nissuer [withheld]\n"
				got, err := os.ReadFile(filepath.Join(dir, "qualify-e2e.log"))
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != want {
					t.Errorf("%s with %q: scrubbed to %q, want %q", sh, tc.pass, got, want)
				}
			}
		})
	}
}

// The host alone is taken from the URL as the provider reads it: trimmed, and
// without an IPv6 address's brackets.
func TestWithholdScrubsThePaddedOrBracketedAPIHost(t *testing.T) {
	for _, tc := range []struct{ api, text, want string }{
		{
			" https://api.hall3.example:443 ",
			"Post \"https://api.hall3.example:443/realms/r/token\": dial tcp: lookup api.hall3.example: no such host\n",
			"Post \"[withheld]/realms/r/token\": dial tcp: lookup [withheld]: no such host\n",
		},
		{
			"https://[fd00::3]:443",
			"Post \"https://[fd00::3]:443/realms/r/token\": x509: certificate is valid for other.example, not fd00::3\n",
			"Post \"[withheld]/realms/r/token\": x509: certificate is valid for other.example, not [withheld]\n",
		},
		{
			"https://kaps:hunter2@api.hall3.example:443",
			"Post \"https://kaps:hunter2@api.hall3.example:443/realms/r/token\": dial tcp: lookup api.hall3.example: no such host\n",
			"Post \"[withheld]/realms/r/token\": dial tcp: lookup [withheld]: no such host\n",
		},
	} {
		for _, sh := range shells(t) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "qualify-e2e.log"), []byte(tc.text), 0o600); err != nil {
				t.Fatal(err)
			}
			l := newSteps(t, sh, map[string]string{"DSX_DATAHALL_3_API": tc.api})
			l.step(true, "withhold", dir)
			got, err := os.ReadFile(filepath.Join(dir, "qualify-e2e.log"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("%s with %q: scrubbed to %q, want %q", sh, tc.api, got, tc.want)
			}
		}
	}
}
