// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nke/cli/internal/bootstrap/manifest"
)

func TestGetBootstrapManifest_Golden(t *testing.T) {
	stdout, _, err := runGenerate(t, "get", "bootstrap-manifest")
	if err != nil {
		t.Fatalf("get bootstrap-manifest: %v", err)
	}
	if stdout != string(manifest.Bytes()) {
		t.Errorf("get bootstrap-manifest output differs from the embedded manifest:\n%s", stdout)
	}
	for _, want := range []string{"components:", "ghcr.io/spiffe/spire-server:1.15.3", "sha256:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("manifest lacks %q:\n%s", want, stdout)
		}
	}
	if _, _, err := runGenerate(t, "get", "bootstrap-manifest", "extra"); err == nil {
		t.Error("get bootstrap-manifest accepted a positional argument")
	}
}
