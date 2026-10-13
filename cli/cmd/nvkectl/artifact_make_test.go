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

// Exercise the actual Make target without compiling or recursively running this suite.
func TestArtifactMakeTargetInputs(t *testing.T) {
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
			if !strings.HasSuffix(string(output), tc.binary+"\n"+tc.version+"\n") {
				t.Fatalf("Make changed the supplied inputs: %s", output)
			}
		})
	}
}
