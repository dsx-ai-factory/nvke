// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// --subject-spec names the subject's spec, so a subject kept apart from the
// tree --root names is looked for where it was pointed, not under the root.
func TestPlanReadsTheSubjectFromTheNamedFile(t *testing.T) {
	// The shipped registry reads the NGC organisation from the environment.
	t.Setenv("QUALIFY_E2E_NGC_ORG", "example-org")
	missing := filepath.Join(t.TempDir(), "elsewhere", "qualification.yaml")

	cmd := planCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{
		"--subject", "other",
		"--root", t.TempDir(),
		"--policy", "../../policy.yaml",
		"--registry", "../../registry.yaml",
		"--subject-spec", missing,
	})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("err = %v, want it to name %s", err, missing)
	}
}
