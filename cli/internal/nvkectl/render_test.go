// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderDeployPath(t *testing.T) {
	spec := filledSiteSpec(t, nil)
	out := filepath.Join(t.TempDir(), "rendered")
	nico := filepath.Join("..", "..", "..", "deploy", "nico")
	// A temp --out is refused unless the test-only allowance is set.
	t.Setenv("NVKECTL_RENDER_ALLOW_TEMP_OUT", "")
	if _, _, err := runGenerate(t, "render", "deploy-path", "--deploy-path", nico, "--sitespec", spec, "--out", out); err == nil || !strings.Contains(err.Error(), "shared temp directory") {
		t.Errorf("temp --out: %v", err)
	}
	t.Setenv("NVKECTL_RENDER_ALLOW_TEMP_OUT", "1")
	stdout, stderr, err := runGenerate(t, "render", "deploy-path", "--deploy-path", nico, "--sitespec", spec, "--out", out)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if !strings.HasPrefix(stdout, "rendered "+nico+" for my-tier0-site into "+out) || !strings.Contains(stdout, out+" holds this site's credentials: controller/site-values.yaml, ") || !strings.HasSuffix(strings.TrimSpace(stdout), "Delete it after the install.") {
		t.Errorf("stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "controller: site values written") || !strings.Contains(stderr, "nvke-base-config: 3 templates rendered") {
		t.Errorf("notes: %q", stderr)
	}
	if _, err := os.Stat(filepath.Join(out, "controller", "site-values.yaml")); err != nil {
		t.Error("controller site values missing")
	}
	// A site spec with placeholders is refused, naming them, and leaves nothing.
	out2 := filepath.Join(t.TempDir(), "rendered")
	_, _, err = runGenerate(t, "render", "deploy-path", "--deploy-path", nico, "--sitespec", generatedSiteSpec(t), "--out", out2)
	if err == nil || !strings.Contains(err.Error(), "REPLACE_* placeholder") {
		t.Errorf("placeholders: %v", err)
	}
	if _, statErr := os.Stat(out2); statErr == nil {
		t.Error("output left behind")
	}
	for _, args := range [][]string{{"--deploy-path", nico, "--out", out2}, {"--sitespec", spec, "--out", out2}, {"--deploy-path", nico, "--sitespec", spec}} {
		if _, _, err := runGenerate(t, append([]string{"render", "deploy-path"}, args...)...); err == nil {
			t.Errorf("accepted without a required flag: %v", args)
		}
	}
}

func TestRenderDeployPath_ArchFromSiteSpec(t *testing.T) {
	t.Setenv("NVKECTL_RENDER_ALLOW_TEMP_OUT", "1")
	spec := filledSiteSpec(t, nil) // controllerStack.arch: arm64
	nico := filepath.Join("..", "..", "..", "deploy", "nico")
	release := filepath.Join(releaseFixtures, "valid.json") // capnico-controller for amd64 and -arm64
	// Without --arch the site spec's architecture selects the artifacts,
	// whatever machine renders. An empty run directory refuses, naming the
	// artifact it looked for: the arm64 capnico digest (e...).
	_, _, err := runGenerate(t, "render", "deploy-path", "--deploy-path", nico, "--sitespec", spec, "--release", release, "--nvkr-run-dir", t.TempDir(), "--out", filepath.Join(t.TempDir(), "rendered"))
	if err == nil || !strings.Contains(err.Error(), "no OCI layout") || !strings.Contains(err.Error(), strings.Repeat("e", 64)) {
		t.Errorf("the arm64 capnico artifact was not selected from the site spec: %v", err)
	}
	// A conflicting --arch is refused.
	_, _, err = runGenerate(t, "render", "deploy-path", "--deploy-path", nico, "--sitespec", spec, "--release", release, "--arch", "linux/amd64", "--out", filepath.Join(t.TempDir(), "r"))
	if err == nil || !strings.Contains(err.Error(), "--arch linux/amd64, but the site spec's controllerStack.arch is arm64") {
		t.Errorf("conflicting arch: %v", err)
	}
}

// nicoStackSiteSpec writes the nico example with its placeholder
// controllerStack section replaced by a filled one borrowed from a generated
// spec, so it is a spec the stack install can read. edit changes the example's
// text first; the files the bootstrap names are not written.
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
	// The example ends with its placeholder controllerStack section and the
	// comment above it; drop both so the filled section is the only one.
	cut := strings.Index(text, "\n# Input for the controller stack install")
	if cut < 0 || !strings.Contains(text[cut:], "\ncontrollerStack:\n") {
		t.Fatal("the nico example's controllerStack section moved; update nicoStackSiteSpec")
	}
	text = text[:cut+1]
	filled, err := os.ReadFile(filledSiteSpec(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	stack := string(filled)
	start, end := strings.Index(stack, "controllerStack:"), strings.Index(stack, "spire:\n  trustDomain")
	if start < 0 || end < start {
		t.Fatalf("the filled site spec has no controllerStack section to borrow:\n%s", stack)
	}
	spec := filepath.Join(t.TempDir(), "sitespec.yaml")
	if err := os.WriteFile(spec, []byte(text+"\n"+stack[start:end]), 0o600); err != nil {
		t.Fatal(err)
	}
	return spec
}

// oneNicoNodeNoVIP makes the nico example a single node with no control-plane
// VIP: valid for the bootstrap, and with no address for the stack install.
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
	t.Setenv("NVKECTL_RENDER_ALLOW_TEMP_OUT", "1")
	nico := filepath.Join("..", "..", "..", "deploy", "nico")
	_, stderr, err := runGenerate(t, "render", "deploy-path", "--deploy-path", nico, "--sitespec", spec, "--out", filepath.Join(t.TempDir(), "rendered"))
	if err == nil || !strings.Contains(err.Error()+stderr, "cluster.controlPlaneVIP") || strings.Contains(err.Error()+stderr, "several nodes") {
		t.Fatalf("err = %v, stderr %q; want the missing VIP named for nico", err, stderr)
	}
}

// TestRenderDeployPath_NicoSpecNeedsNoBootstrapFiles: the controller stack
// install on the site node renders from a nico site spec without the NICo token
// credentials file and the release manifests, which only the bootstrap reads.
func TestRenderDeployPath_NicoSpecNeedsNoBootstrapFiles(t *testing.T) {
	spec := nicoStackSiteSpec(t, nil)
	// Nothing the bootstrap reads sits beside the spec, and the credentials
	// variable is unset, as on the site node.
	t.Setenv("NVKECTL_NICO_CREDENTIALS", "")
	for _, name := range []string{"bootstrap-releases"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(spec), name)); err == nil {
			t.Fatalf("%s exists beside the spec; the test needs it absent", name)
		}
	}

	t.Setenv("NVKECTL_RENDER_ALLOW_TEMP_OUT", "1")
	out := filepath.Join(t.TempDir(), "rendered")
	nico := filepath.Join("..", "..", "..", "deploy", "nico")
	if _, stderr, err := runGenerate(t, "render", "deploy-path", "--deploy-path", nico, "--sitespec", spec, "--out", out); err != nil {
		t.Fatalf("render deploy-path needed a bootstrap-only file: %v\n%s", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(out, "controller", "site-values.yaml")); err != nil {
		t.Error("controller site values missing")
	}
}
