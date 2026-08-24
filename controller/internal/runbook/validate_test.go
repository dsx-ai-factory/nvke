// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runbook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateProfileReferences(t *testing.T) {
	profilesDir := t.TempDir()
	runbooksDir := filepath.Join(profilesDir, "runbooks")
	configProfilesDir := filepath.Join(profilesDir, "configProfiles")
	require.NoError(t, os.MkdirAll(runbooksDir, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(configProfilesDir, "deprecated"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(configProfilesDir, "active.yaml"),
		[]byte("hooks:\n  beforeJoinNode:\n    - runbook:\n        name: configure-host\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(configProfilesDir, "deprecated", "old.yaml"),
		[]byte("hooks:\n  afterRemoveNode:\n    - runbook:\n        name: cleanup-host\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(filepath.Join(runbooksDir, "configure-host.sh"), []byte("true\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(runbooksDir, "cleanup-host.sh"), []byte("true\n"), 0o644))

	require.NoError(t, ValidateProfileReferences(profilesDir, runbooksDir))
}

func TestValidateProfileReferencesRequiresExternalScript(t *testing.T) {
	profilesDir := t.TempDir()
	configProfilesDir := filepath.Join(profilesDir, "configProfiles")
	require.NoError(t, os.MkdirAll(configProfilesDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(configProfilesDir, "profile.yaml"),
		[]byte("hooks:\n  beforeJoinNode:\n    - runbook:\n        name: reboot\n"),
		0o644,
	))

	err := ValidateProfileReferences(profilesDir, filepath.Join(profilesDir, "runbooks"))

	require.ErrorContains(t, err, `references invalid runbook "reboot"`)
}

func TestValidateProfileReferencesRejectsInvalidName(t *testing.T) {
	profilesDir := t.TempDir()
	configProfilesDir := filepath.Join(profilesDir, "configProfiles")
	require.NoError(t, os.MkdirAll(configProfilesDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(configProfilesDir, "profile.yaml"),
		[]byte("hooks:\n  afterJoinNode:\n    - runbook:\n        name: ../reboot\n"),
		0o644,
	))

	err := ValidateProfileReferences(profilesDir, filepath.Join(profilesDir, "runbooks"))

	require.ErrorContains(t, err, `invalid runbook name "../reboot"`)
}
