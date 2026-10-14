// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"path/filepath"
	"strings"
	"testing"
)

var releaseFixtures = filepath.Join("..", "..", "..", "deploy", "testdata", "nvkr-release")

func TestInspectNVKRRelease_Artifacts(t *testing.T) {
	valid := filepath.Join(releaseFixtures, "valid.json")
	stdout, _, err := runGenerate(t, "inspect", "nvkr-release", valid, "--artifacts")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 15 {
		t.Fatalf("lines: %d", len(lines))
	}
	aaaa := "sha256:" + strings.Repeat("a", 64)
	if lines[0] != "cwe\toci\tghcr.io/example/recipes/cwe@"+aaaa+"\t"+aaaa+"\tlinux/amd64,linux/arm64\t" {
		t.Errorf("first line: %q", lines[0])
	}
	if !strings.HasPrefix(lines[2], "capnico-controller-arm64\toci\t") || !strings.HasSuffix(lines[2], "\tlinux/arm64\t") {
		t.Errorf("arch column: %q", lines[2])
	}
	stdout, _, err = runGenerate(t, "inspect", "nvkr-release", valid, "--artifacts", "--kind", "helm-oci")
	if err != nil || strings.Count(stdout, "\n") != 6 || !strings.HasPrefix(stdout, "controller-chart\thelm-oci\t") || !strings.Contains(stdout, "\t0.1.1\n") {
		t.Errorf("--kind helm-oci: %q %v", stdout, err)
	}
	for _, args := range [][]string{{"--kind", "oci"}, {}, {"--artifacts", "--meta"}, {"--select", "cwe"}, {"--arch", "linux/arm64"}} {
		if _, _, err := runGenerate(t, append([]string{"inspect", "nvkr-release", valid}, args...)...); err == nil {
			t.Errorf("accepted: %v", args)
		}
	}
}

func TestInspectNVKRRelease_Meta(t *testing.T) {
	stdout, _, err := runGenerate(t, "inspect", "nvkr-release", filepath.Join(releaseFixtures, "valid.json"), "--meta")
	if err != nil {
		t.Fatal(err)
	}
	want := "REFERENCE='ghcr.io/example/recipes/releases@sha256:" + strings.Repeat("0", 64) + "'\n" +
		"ROOT_DIGEST='sha256:" + strings.Repeat("0", 64) + "'\n" +
		"QUALIFICATION='pipeline-attested'\nCONTENT_SUBJECT='sha256:" + strings.Repeat("c", 64) + "'\n"
	if stdout != want {
		t.Errorf("meta:\n%s", stdout)
	}
}

func TestInspectNVKRRelease_Select(t *testing.T) {
	valid := filepath.Join(releaseFixtures, "valid.json")
	stdout, _, err := runGenerate(t, "inspect", "nvkr-release", valid, "--select", "capnico-controller", "--arch", "linux/arm64")
	eeee := "sha256:" + strings.Repeat("e", 64)
	if err != nil || stdout != "capnico-controller-arm64\tghcr.io/example/recipes/capnico-controller-arm64@"+eeee+"\t"+eeee+"\n" {
		t.Errorf("select arm64: %q %v", stdout, err)
	}
	stdout, _, err = runGenerate(t, "inspect", "nvkr-release", valid, "--select", "absent", "--arch", "linux/arm64")
	if err != nil || stdout != "" {
		t.Errorf("an absent artifact prints nothing and exits 0: %q %v", stdout, err)
	}
	_, _, err = runGenerate(t, "inspect", "nvkr-release", valid, "--select", "spire-server-image", "--arch", "linux/amd64")
	if err == nil || !strings.Contains(err.Error(), "published only for other architectures") {
		t.Errorf("other arch: %v", err)
	}
	_, _, err = runGenerate(t, "inspect", "nvkr-release", filepath.Join(releaseFixtures, "bad-schema.json"), "--meta")
	if err == nil || !strings.Contains(err.Error(), "bad-schema.json") {
		t.Errorf("bad file: %v", err)
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
