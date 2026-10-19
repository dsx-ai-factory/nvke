// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestArtifactMakeTargetInputs checks the Make target's required inputs and path handling.
func TestArtifactMakeTargetInputs(t *testing.T) {
	cliDir, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	probe := filepath.Join(dir, "probe.sh")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nprintf '%s\\n' \"$NVKECTL\" \"$NVKECTL_EXPECTED_VERSION\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		binary  string
		version string
		wantErr bool
	}{
		{name: "absolute spaced path", binary: filepath.Join(dir, "space path", "nvkectl"), version: "v1"},
		{name: "relative spaced path", binary: "space path/nvkectl", version: "v1"},
		{name: "relative path", binary: "bin/nvkectl", version: "v1"},
		{name: "missing artifact", version: "v1", wantErr: true},
		{name: "missing version", binary: "space path/nvkectl", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("make", "--no-print-directory", "-C", "../..", "test-artifact",
				"GO="+probe, "NVKECTL="+tc.binary, "NVKECTL_EXPECTED_VERSION="+tc.version)
			output, err := cmd.CombinedOutput()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected required-input rejection, got %s", output)
				}
				return
			}
			if err != nil {
				t.Fatalf("make test-artifact: %v\n%s", err, output)
			}
			wantBinary := tc.binary
			if !filepath.IsAbs(wantBinary) {
				wantBinary = filepath.Join(cliDir, wantBinary)
			}
			if !strings.HasSuffix(string(output), wantBinary+"\n"+tc.version+"\n") {
				t.Fatalf("expected binary %q and version %q, got: %s", wantBinary, tc.version, output)
			}
		})
	}
}
