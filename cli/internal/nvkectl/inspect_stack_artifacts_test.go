// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"path/filepath"
	"strings"
	"testing"
)

var artifactFixtures = filepath.Join("..", "..", "..", "deploy", "testdata", "stack-artifacts")

func TestInspectStackArtifacts_Artifacts(t *testing.T) {
	valid := filepath.Join(artifactFixtures, "valid.json")
	stdout, _, err := runGenerate(t, "inspect", "stack-artifacts", valid, "--artifacts")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 15 {
		t.Fatalf("lines: %d", len(lines))
	}
	aaaa := "sha256:" + strings.Repeat("a", 64)
	if lines[0] != "cwe\toci\tghcr.io/example/recipes/cwe@"+aaaa+"\t"+aaaa+"\tlinux/amd64,linux/arm64\t\t" {
		t.Errorf("first line: %q", lines[0])
	}
	if !strings.HasPrefix(lines[2], "capnico-controller-arm64\toci\t") || !strings.HasSuffix(lines[2], "\tlinux/arm64\t\t") {
		t.Errorf("arch column: %q", lines[2])
	}
	stdout, _, err = runGenerate(t, "inspect", "stack-artifacts", valid, "--artifacts", "--kind", "helm-oci")
	if err != nil || strings.Count(stdout, "\n") != 6 || !strings.HasPrefix(stdout, "controller-chart\thelm-oci\t") || !strings.Contains(stdout, "\t0.1.1\t\n") {
		t.Errorf("--kind helm-oci: %q %v", stdout, err)
	}
	// The layout column is absolute.
	local := filepath.Join(artifactFixtures, "local-images.json")
	abs, _ := filepath.Abs(filepath.Join(artifactFixtures, "layouts", "cwe"))
	stdout, _, err = runGenerate(t, "inspect", "stack-artifacts", local, "--artifacts", "--kind", "oci")
	if err != nil || !strings.HasPrefix(stdout, "cwe\toci\tnvke.local/cwe@"+aaaa+"\t"+aaaa+"\t\t\t"+abs+"\n") {
		t.Errorf("layout column: %q %v", stdout, err)
	}
	for _, args := range [][]string{{"--kind", "oci"}, {}, {"--artifacts", "--meta"}, {"--select", "cwe"}, {"--arch", "linux/arm64"}, {"--bindings"}, {"--bindings", "--arch", "linux/arm64"}, {"--deploy-path", "x", "--meta"}} {
		if _, _, err := runGenerate(t, append([]string{"inspect", "stack-artifacts", valid}, args...)...); err == nil {
			t.Errorf("accepted: %v", args)
		}
	}
	if _, _, err := runGenerate(t, "inspect", "nvkr-release", valid, "--artifacts"); err == nil {
		t.Error("inspect nvkr-release still exists")
	}
}

func TestInspectStackArtifacts_Meta(t *testing.T) {
	valid := filepath.Join(artifactFixtures, "valid.json")
	stdout, _, err := runGenerate(t, "inspect", "stack-artifacts", valid, "--meta")
	if err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(valid)
	want := "STACK_ARTIFACTS='" + abs + "'\n" +
		"SOURCE_REFERENCE='ghcr.io/example/recipes/releases@sha256:" + strings.Repeat("0", 64) + "'\n" +
		"QUALIFICATION='pipeline-attested'\nARTIFACTS='15'\nLAYOUTS='0'\nLAYOUTS_MISSING=''\nPULL_HOSTS='ghcr.io'\n"
	if stdout != want {
		t.Errorf("meta:\n%s\nwant:\n%s", stdout, want)
	}
	// Without a source block the two values are empty; layouts are counted
	// and their hosts are not pull hosts.
	stdout, _, err = runGenerate(t, "inspect", "stack-artifacts", filepath.Join(artifactFixtures, "local-images.json"), "--meta")
	if err != nil || !strings.Contains(stdout, "SOURCE_REFERENCE=''\nQUALIFICATION=''\nARTIFACTS='4'\nLAYOUTS='4'\nLAYOUTS_MISSING=''\nPULL_HOSTS=''\n") {
		t.Errorf("no source, all layouts:\n%s %v", stdout, err)
	}
	// A missing layout is named, and the command still exits 0: the check
	// reports it as a finding.
	stdout, _, err = runGenerate(t, "inspect", "stack-artifacts", filepath.Join(artifactFixtures, "layout-missing.json"), "--meta")
	if err != nil || !strings.Contains(stdout, "LAYOUTS='1'\nLAYOUTS_MISSING='cwe'\n") {
		t.Errorf("missing layout:\n%s %v", stdout, err)
	}
	// Values are quoted for eval, whatever they hold.
	if got := shellQuote("a'b$(x)"); got != `'a'\''b$(x)'` {
		t.Errorf("shellQuote: %s", got)
	}
}

func TestInspectStackArtifacts_Select(t *testing.T) {
	valid := filepath.Join(artifactFixtures, "valid.json")
	stdout, _, err := runGenerate(t, "inspect", "stack-artifacts", valid, "--select", "capnico-controller", "--arch", "linux/arm64")
	eeee := "sha256:" + strings.Repeat("e", 64)
	if err != nil || stdout != "capnico-controller-arm64\tghcr.io/example/recipes/capnico-controller-arm64@"+eeee+"\t"+eeee+"\t\n" {
		t.Errorf("select arm64: %q %v", stdout, err)
	}
	stdout, _, err = runGenerate(t, "inspect", "stack-artifacts", valid, "--select", "absent", "--arch", "linux/arm64")
	if err != nil || stdout != "" {
		t.Errorf("an absent artifact prints nothing and exits 0: %q %v", stdout, err)
	}
	_, _, err = runGenerate(t, "inspect", "stack-artifacts", valid, "--select", "spire-server-image", "--arch", "linux/amd64")
	if err == nil || !strings.Contains(err.Error(), "published only for other architectures") {
		t.Errorf("other arch: %v", err)
	}
	_, _, err = runGenerate(t, "inspect", "stack-artifacts", filepath.Join(artifactFixtures, "bad-schema.json"), "--meta")
	if err == nil || !strings.Contains(err.Error(), "bad-schema.json") {
		t.Errorf("bad file: %v", err)
	}
	_, _, err = runGenerate(t, "inspect", "stack-artifacts", filepath.Join("..", "..", "..", "hack", "testdata", "nvkr-release", "valid.json"), "--meta")
	if err == nil || !strings.Contains(err.Error(), "convert it with hack/nvkr-run-to-stack-artifacts.py") {
		t.Errorf("nvkr export: %v", err)
	}
}

func TestInspectStackArtifacts_Bindings(t *testing.T) {
	nico := filepath.Join("..", "..", "..", "deploy", "nico")
	stdout, _, err := runGenerate(t, "inspect", "stack-artifacts", filepath.Join(artifactFixtures, "valid.json"), "--bindings", "--deploy-path", nico, "--arch", "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"capnico\timage\tcapnico-controller\tcapnico-controller-arm64\tbound\n",
		"controller\tchart\tcontroller-chart\tcontroller-chart\tbound\n",
		"controller\timage\tcwe\tcwe\tbound\n",
		"controller\tprofiles-image\tcontroller-profiles\tcontroller-profiles\tbound\n",
		"temporal\tchart\ttemporal\ttemporal\tbound\n",
		"unused\tnvkectl\n",
		"unused\tnvke-os\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("want %q in:\n%s", want, stdout)
		}
	}
	// The amd64 CAPNICo is a sibling of a bound name, not unused.
	if strings.Contains(stdout, "unused\tcapnico-controller") {
		t.Errorf("a sibling is listed as unused:\n%s", stdout)
	}
	// Three local images: every chart is committed, and an arm64 render of
	// an amd64-only file names the architecture.
	stdout, _, err = runGenerate(t, "inspect", "stack-artifacts", filepath.Join(artifactFixtures, "local-images-amd64.json"), "--bindings", "--deploy-path", nico, "--arch", "linux/arm64")
	if err != nil || !strings.Contains(stdout, "capnico\timage\tcapnico-controller\t-\tother-arch:linux/amd64\n") || !strings.Contains(stdout, "temporal\tchart\ttemporal\t-\tcommitted\n") || strings.Contains(stdout, "unused\t") {
		t.Errorf("amd64 file for arm64:\n%s %v", stdout, err)
	}
	stdout, _, err = runGenerate(t, "inspect", "stack-artifacts", filepath.Join(artifactFixtures, "no-cwe.json"), "--bindings", "--deploy-path", nico, "--arch", "linux/arm64")
	if err != nil || !strings.Contains(stdout, "controller\timage\tcwe\t-\tmissing\n") {
		t.Errorf("missing image:\n%s %v", stdout, err)
	}
	if _, _, err := runGenerate(t, "inspect", "stack-artifacts", filepath.Join(artifactFixtures, "valid.json"), "--bindings", "--deploy-path", nico, "--arch", "linux/mips"); err == nil {
		t.Error("bad --arch accepted")
	}
	if _, _, err := runGenerate(t, "inspect", "stack-artifacts", filepath.Join(artifactFixtures, "valid.json"), "--bindings", "--deploy-path", t.TempDir(), "--arch", "linux/arm64"); err == nil {
		t.Error("a directory without nvke-deploy.yaml accepted")
	}
}

func TestInspectIsHidden(t *testing.T) {
	stdout, _, err := runGenerate(t, "--help")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "\n  inspect ") {
		t.Error("inspect is listed in the top-level help")
	}
}
