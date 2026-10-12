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
	stdout, stderr, err := runGenerate(t, "render", "deploy-path", "--deploy-path", nico, "--sitespec", spec, "--out", out)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if !strings.HasPrefix(stdout, "rendered "+nico+" for my-tier0-site into "+out) {
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
