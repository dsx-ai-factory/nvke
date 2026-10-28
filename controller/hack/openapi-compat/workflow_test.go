// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Execute the actual workflow step, substituting only make to observe which
// owning checker/baseline it invokes. Git still verifies the target commit.
func TestWorkflowCompatibilityBaseline(t *testing.T) {
	raw, err := os.ReadFile("../../../.github/workflows/api-compat.yml")
	require.NoError(t, err)
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &workflow))
	var script string
	for _, step := range workflow.Jobs["api-compat"].Steps {
		if step.Name == "Check the specification against its target compatibility baseline" {
			script = step.Run
		}
	}
	require.NotEmpty(t, script)
	commit, err := exec.CommandContext(t.Context(), "git", "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	sha := strings.TrimSpace(string(commit))
	for _, tc := range []struct {
		name, event, defaultBranch, base, baseSHA string
		wantBaseline, pass                        bool
	}{
		{"main PR uses published tag", "pull_request", "main", "main", sha, false, true},
		{"different default branch", "pull_request", "trunk", "trunk", sha, false, true},
		{"stacked PR uses target commit", "pull_request", "main", "fix/pinned-harness", sha, true, true},
		{"maintenance PR uses target commit", "pull_request", "main", "maint/nvkr-candidate", sha, true, true},
		{"push uses published tag", "push", "main", "", "", false, true},
		{"merge queue uses published tag", "merge_group", "main", "", "", false, true},
		{"manual uses published tag", "workflow_dispatch", "main", "", "", false, true},
		{"missing default branch", "pull_request", "", "fix/pinned-harness", sha, false, false},
		{"missing target branch", "pull_request", "main", "", sha, false, false},
		{"missing target commit", "pull_request", "main", "fix/pinned-harness", "", false, false},
		{"invalid target commit", "pull_request", "main", "fix/pinned-harness", "HEAD; exit 0", false, false},
		{"unknown target commit", "pull_request", "main", "fix/pinned-harness", strings.Repeat("f", 40), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "arguments")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "make"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >\"$TEST_ARGUMENTS\"\n"), 0o700))
			command := exec.CommandContext(t.Context(), "bash", "-ec", script)
			command.Env = []string{
				"PATH=" + dir + ":" + os.Getenv("PATH"), "TEST_ARGUMENTS=" + marker,
				"API_COMPAT_EVENT=" + tc.event, "API_COMPAT_DEFAULT_BRANCH=" + tc.defaultBranch,
				"API_COMPAT_BASE_REF=" + tc.base, "API_COMPAT_BASE_SHA=" + tc.baseSHA,
			}
			out, runErr := command.CombinedOutput()
			if !tc.pass {
				require.Error(t, runErr, "%s", out)
				_, statErr := os.Stat(marker)
				require.True(t, os.IsNotExist(statErr), "invalid metadata reached checker")
				return
			}
			require.NoError(t, runErr, "%s", out)
			arguments, readErr := os.ReadFile(marker)
			require.NoError(t, readErr)
			want := "api-compat-check\nAPI_COMPAT_REPORT=.local/openapi-changes.json\n"
			if tc.wantBaseline {
				want += "API_COMPAT_BASELINE=" + sha + "\n"
			}
			require.Equal(t, want, string(arguments))
		})
	}
}
