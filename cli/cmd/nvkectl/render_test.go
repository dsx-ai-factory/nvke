// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

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
	if !strings.Contains(stderr, "controller: site values written") || !strings.Contains(stderr, "nvke-base-config: 4 templates rendered") {
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
