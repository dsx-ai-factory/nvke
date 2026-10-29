// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nvke/cli/internal/deploypath/stackconfig"
)

// stackConfigExample is the operator's starting point for the stack config.
var stackConfigExample = filepath.Join("..", "..", "..", "deploy", "examples", "stack-config.json")

// filledStackConfig writes the example stack config with every placeholder
// replaced, in a directory of its own, sets the install's credentials in
// the environment, and returns the file's path. edits replace further text.
func filledStackConfig(t *testing.T, edits map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "stack")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(stackconfig.EnvNicoToken, "placeholder")
	t.Setenv(stackconfig.EnvNodeUser, "placeholder")
	t.Setenv(stackconfig.EnvNodePassword, "placeholder")
	t.Setenv(stackconfig.EnvDatabasePassword, "placeholder")
	body, err := os.ReadFile(stackConfigExample)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"REPLACE_NICO_ENDPOINT": "https://nico.example.com", "REPLACE_NICO_ORG_ID": "org-1", "REPLACE_NICO_API_NAME": "forge",
		// deploy/nico enables profiles.image without a pin.
		`"profiles": ""`: `"profiles": "ghcr.io/example/controller-profiles:v1"`,
	}
	for k, v := range edits {
		values[k] = v
	}
	text := string(body)
	for placeholder, value := range values {
		text = strings.ReplaceAll(text, placeholder, value)
	}
	path := filepath.Join(dir, "stack-config.json")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRenderDeployPath(t *testing.T) {
	spec := siteSpecWithTrustDomain(t)
	stack := filledStackConfig(t, nil)
	out := filepath.Join(t.TempDir(), "rendered")
	nico := filepath.Join("..", "..", "..", "deploy", "nico")
	// A temp --out is refused unless the test-only allowance is set.
	t.Setenv("NVKECTL_RENDER_ALLOW_TEMP_OUT", "")
	if _, _, err := runGenerate(t, "render", "deploy-path", "--deploy-path", nico, "--sitespec", spec, "--stack-config", stack, "--arch", "linux/arm64", "--out", out); err == nil || !strings.Contains(err.Error(), "shared temp directory") {
		t.Errorf("temp --out: %v", err)
	}
	t.Setenv("NVKECTL_RENDER_ALLOW_TEMP_OUT", "1")
	stdout, stderr, err := runGenerate(t, "render", "deploy-path", "--deploy-path", nico, "--sitespec", spec, "--stack-config", stack, "--arch", "linux/arm64", "--out", out)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if !strings.HasPrefix(stdout, "rendered "+nico+" for my-tier0-site into "+out) || !strings.Contains(stdout, out+" holds this site's credentials: site-credentials/secrets.yaml, nvke-base-config/20-nico-credentials.yaml, ") || !strings.HasSuffix(strings.TrimSpace(stdout), "Delete it after the install.") {
		t.Errorf("stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "controller: site values written") || !strings.Contains(stderr, "nvke-base-config: 3 templates rendered") {
		t.Errorf("notes: %q", stderr)
	}
	if _, err := os.Stat(filepath.Join(out, "controller", "site-values.yaml")); err != nil {
		t.Error("controller site values missing")
	}
	// A stack config with placeholders is refused, naming the file and the
	// placeholders, and leaves nothing.
	out2 := filepath.Join(t.TempDir(), "rendered")
	_, _, err = runGenerate(t, "render", "deploy-path", "--deploy-path", nico, "--sitespec", spec, "--stack-config", stackConfigExample, "--arch", "linux/arm64", "--out", out2)
	if err == nil || !strings.Contains(err.Error(), "stack config "+stackConfigExample) || !strings.Contains(err.Error(), "nico.endpoint: still a REPLACE_* placeholder") {
		t.Errorf("placeholders: %v", err)
	}
	if _, statErr := os.Stat(out2); statErr == nil {
		t.Error("output left behind")
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no --deploy-path":  {[]string{"--sitespec", spec, "--stack-config", stack, "--out", out2}, "deploy-path"},
		"no --out":          {[]string{"--deploy-path", nico, "--sitespec", spec, "--stack-config", stack}, "out"},
		"no input":          {[]string{"--deploy-path", nico, "--out", out2}, "pass --sitespec with --stack-config, --release, or both"},
		"no --stack-config": {[]string{"--deploy-path", nico, "--sitespec", spec, "--out", out2}, "--sitespec needs --stack-config: the stack's input is the stack config file"},
		"no --sitespec":     {[]string{"--deploy-path", nico, "--stack-config", stack, "--out", out2}, "--stack-config needs --sitespec: the site name, VIP and trust domain come from the site spec"},
	} {
		_, _, err := runGenerate(t, append([]string{"render", "deploy-path"}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
		if _, statErr := os.Stat(out2); statErr == nil {
			t.Errorf("%s: output left behind", name)
		}
	}
}

func TestRenderDeployPath_Arch(t *testing.T) {
	t.Setenv("NVKECTL_RENDER_ALLOW_TEMP_OUT", "1")
	spec := siteSpecWithTrustDomain(t)
	stack := filledStackConfig(t, nil)
	nico := filepath.Join("..", "..", "..", "deploy", "nico")
	release := filepath.Join(releaseFixtures, "valid.json") // capnico-controller for amd64 and -arm64
	// --arch selects the release artifacts. An empty run directory refuses,
	// naming the artifact it looked for: the arm64 capnico digest (e...).
	_, _, err := runGenerate(t, "render", "deploy-path", "--deploy-path", nico, "--sitespec", spec, "--stack-config", stack, "--arch", "linux/arm64", "--release", release, "--nvkr-run-dir", t.TempDir(), "--out", filepath.Join(t.TempDir(), "rendered"))
	if err == nil || !strings.Contains(err.Error(), "no OCI layout") || !strings.Contains(err.Error(), strings.Repeat("e", 64)) {
		t.Errorf("the arm64 capnico artifact was not selected from --arch: %v", err)
	}
	// A value outside the two architectures is refused before anything is read.
	_, _, err = runGenerate(t, "render", "deploy-path", "--deploy-path", nico, "--sitespec", spec, "--stack-config", stack, "--arch", "linux/x86", "--out", filepath.Join(t.TempDir(), "r"))
	if err == nil || !strings.Contains(err.Error(), "--arch linux/x86 is not one of linux/amd64, linux/arm64") {
		t.Errorf("bad --arch: %v", err)
	}
	// The TPM attestor checksum follows --arch; without the flag the host's
	// architecture is used and said so.
	checksums := map[string]string{"linux/amd64": "46d0caad8c25a027dd11c93e18b58a8bc6fbd9f1fe2e36fa2a0dd440986de4dc", "linux/arm64": "b69d0153c8241383f24da138bc85429a620d9f962a2f10abb314aa0e39046d16"}
	for arch, want := range checksums {
		out := filepath.Join(t.TempDir(), "rendered")
		_, stderr, err := runGenerate(t, "render", "deploy-path", "--deploy-path", nico, "--sitespec", spec, "--stack-config", stack, "--arch", arch, "--out", out)
		if err != nil {
			t.Fatalf("%s: %v\n%s", arch, err, stderr)
		}
		if strings.Contains(stderr, "warning: --arch not given") {
			t.Errorf("%s: the host-default warning fired with --arch given:\n%s", arch, stderr)
		}
		if got := tpmChecksum(t, out); got != want {
			t.Errorf("%s: TPM checksum %s, want %s", arch, got, want)
		}
	}
	out := filepath.Join(t.TempDir(), "rendered")
	_, stderr, err := runGenerate(t, "render", "deploy-path", "--deploy-path", nico, "--sitespec", spec, "--stack-config", stack, "--out", out)
	if err != nil {
		t.Fatalf("without --arch: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "warning: --arch not given; rendering for the host's linux/") {
		t.Errorf("no host-default warning:\n%s", stderr)
	}
	// A release alone keeps rendering, as the deploy suite does, without a warning.
	_, stderr, err = runGenerate(t, "render", "deploy-path", "--deploy-path", nico, "--release", release, "--arch", "linux/arm64", "--nvkr-run-dir", t.TempDir(), "--out", filepath.Join(t.TempDir(), "r"))
	if err == nil || !strings.Contains(err.Error(), "no OCI layout") || strings.Contains(stderr, "warning: --arch") {
		t.Errorf("release alone: %v\n%s", err, stderr)
	}
}

// nicoStackSiteSpec writes the nico example, which the controller stack install
// reads beside a stack config. edit changes the example's text first; the
// files the bootstrap names are not written.
func nicoStackSiteSpec(t *testing.T, edit func(string) string) string {
	t.Helper()
	example, err := os.ReadFile(filepath.Join("..", "..", "examples", "sitespec-nico.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(example)
	if edit != nil {
		text = edit(text)
	}
	spec := filepath.Join(t.TempDir(), "sitespec.yaml")
	if err := os.WriteFile(spec, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return spec
}

// oneNicoNodeNoVIP makes the nico example a single node with no control-plane
// VIP, which a nico site spec must have.
func oneNicoNodeNoVIP(t *testing.T) func(string) string {
	return func(text string) string {
		t.Helper()
		out := strings.Replace(text, "count: 3", "count: 1", 1)
		out = strings.Replace(out, `  controlPlaneVIP: "10.0.0.100"`+"\n", "", 1)
		if out == text || strings.Contains(out, "controlPlaneVIP: ") {
			t.Fatal("the nico example changed; update oneNicoNodeNoVIP")
		}
		return out
	}
}

// TestRenderDeployPath_NicoOneNodeWithoutVIP: render deploy-path says what the
// nico spec lacks, not that it has "several nodes".
func TestRenderDeployPath_NicoOneNodeWithoutVIP(t *testing.T) {
	spec := nicoStackSiteSpec(t, oneNicoNodeNoVIP(t))
	stack := filledStackConfig(t, nil)
	t.Setenv("NVKECTL_RENDER_ALLOW_TEMP_OUT", "1")
	nico := filepath.Join("..", "..", "..", "deploy", "nico")
	_, stderr, err := runGenerate(t, "render", "deploy-path", "--deploy-path", nico, "--sitespec", spec, "--stack-config", stack,
		"--arch", "linux/arm64", "--out", filepath.Join(t.TempDir(), "rendered"))
	if err == nil || !strings.Contains(err.Error()+stderr, "cluster.controlPlaneVIP") || strings.Contains(err.Error()+stderr, "several nodes") {
		t.Fatalf("err = %v, stderr %q; want the missing VIP named for nico", err, stderr)
	}
}

// TestRenderDeployPath_NicoSpecNeedsNoBootstrapFiles: the controller stack
// install on the site node renders from a nico site spec without the NICo token
// credentials file and the release manifests, which only the bootstrap reads.
func TestRenderDeployPath_NicoSpecNeedsNoBootstrapFiles(t *testing.T) {
	spec := nicoStackSiteSpec(t, nil)
	stack := filledStackConfig(t, nil)
	// Nothing the bootstrap reads sits beside the spec, and the credentials
	// variable is unset, as on the site node.
	t.Setenv("NVKECTL_NICO_CREDENTIALS", "")
	if _, err := os.Stat(filepath.Join(filepath.Dir(spec), "bootstrap-releases")); err == nil {
		t.Fatal("bootstrap-releases exists beside the spec; the test needs it absent")
	}

	t.Setenv("NVKECTL_RENDER_ALLOW_TEMP_OUT", "1")
	out := filepath.Join(t.TempDir(), "rendered")
	nico := filepath.Join("..", "..", "..", "deploy", "nico")
	if _, stderr, err := runGenerate(t, "render", "deploy-path", "--deploy-path", nico, "--sitespec", spec, "--stack-config", stack,
		"--arch", "linux/arm64", "--out", out); err != nil {
		t.Fatalf("render deploy-path needed a bootstrap-only file: %v\n%s", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(out, "controller", "site-values.yaml")); err != nil {
		t.Error("controller site values missing")
	}
}

// tpmChecksum reads the SPIRE TPM attestor checksum from a rendered tree.
func tpmChecksum(t *testing.T, out string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(out, "spire", "site-values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Server struct {
			NodeAttestor struct {
				TPMDirect struct {
					Checksum string `yaml:"checksum"`
				} `yaml:"tpmDirect"`
			} `yaml:"nodeAttestor"`
		} `yaml:"spire-server"`
	}
	if err := yaml.Unmarshal(data, &values); err != nil {
		t.Fatal(err)
	}
	return values.Server.NodeAttestor.TPMDirect.Checksum
}
