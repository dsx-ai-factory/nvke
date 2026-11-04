// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// These test release/hack/qualify-e2e.sh, the steps the qualification
// workflow and `qualify local` both run. Only the steps that reach nothing
// past git are run here: the ones that pull, build or take a machine are the
// workflow's to prove.
package qualification_test

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
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

func TestMatrixHasOneEntryPerStream(t *testing.T) {
	p := plan(t)
	for _, sh := range shells(t) {
		out, err := exec.CommandContext(t.Context(), sh, script, "matrix", p).Output()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(string(out), `"stream":`); got != 2 {
			t.Errorf("matrix has %d entries, want 2: %s", got, out)
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
			"CAPABILITIES":               "arch=amd64,gpu=l40s",
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
		site, tenant string
		want         string
	}{
		{"false", "false", ""},
		{"true", "false", "lifecycle:\n  preserveOnFailure: true\n"},
		{"false", "true", "lifecycle:\n  skipDelete: true\n"},
		{"true", "true", "lifecycle:\n  skipDelete: true\n"},
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
				t.Errorf("%s keep-site=%s keep-tenant=%s: lifecycle = %q, want %q", sh, tc.site, tc.tenant, got, tc.want)
			}
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
