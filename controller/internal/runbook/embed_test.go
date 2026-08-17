// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runbook

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// Assert that one random runbook is embedded.
// The actual runbook chosen is not important.
func TestScript(t *testing.T) {
	script, err := Script("", "reboot")

	require.NoError(t, err)
	require.Contains(t, script, "Rebooting the system now")
}

func TestScriptPrefersExternalRunbook(t *testing.T) {
	runbooksDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(runbooksDir, "reboot.sh"), []byte("external reboot\n"), 0o644))

	script, err := Script(runbooksDir, "reboot")

	require.NoError(t, err)
	require.Equal(t, "external reboot\n", script)
}

func TestScriptFallsBackToEmbeddedRunbook(t *testing.T) {
	script, err := Script(t.TempDir(), "reboot")

	require.NoError(t, err)
	require.Contains(t, script, "Rebooting the system now")
}

func TestScriptRejectsInvalidName(t *testing.T) {
	_, err := Script(t.TempDir(), "../reboot")

	require.EqualError(t, err, `invalid runbook name "../reboot"`)
}

func TestScriptReportsMissingRunbook(t *testing.T) {
	_, err := Script(t.TempDir(), "missing-runbook")

	require.ErrorContains(t, err, `runbook "missing-runbook" not found`)
}

func TestNames(t *testing.T) {
	names := Names()

	require.NotEmpty(t, names)
	require.Contains(t, names, "reboot")
	require.True(t, sort.StringsAreSorted(names))
}
