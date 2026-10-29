// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"cmp"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/runbook"
)

func TestGetConfigProfile(t *testing.T) {
	t.Run("returns parsed profile when file exists", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "configProfiles"), 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "configProfiles", "my-profile.yaml"),
			[]byte(`hooks:
  beforeRemoveNode:
    - runbook:
        name: pre-drain-evacuate-infra
        env:
          FOO: bar
`),
			0o644,
		))

		profile, missing, err := getConfigProfile(dir, "my-profile")

		require.NoError(t, err)
		assert.False(t, missing)
		require.Len(t, profile.Hooks.BeforeRemoveNode, 1)
		assert.Equal(t, "pre-drain-evacuate-infra", profile.Hooks.BeforeRemoveNode[0].Runbook.Name)
		assert.Equal(t, map[string]string{"FOO": "bar"}, profile.Hooks.BeforeRemoveNode[0].Runbook.Env)
	})

	t.Run("falls back to deprecated dir and returns hooks when active file is absent", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "configProfiles", "deprecated"), 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "configProfiles", "deprecated", "old-profile.yaml"),
			[]byte(`hooks:
  beforeRemoveNode:
    - runbook:
        name: cleanup-old-stuff
`),
			0o644,
		))

		profile, missing, err := getConfigProfile(dir, "old-profile")

		require.NoError(t, err)
		assert.False(t, missing)
		require.Len(t, profile.Hooks.BeforeRemoveNode, 1)
		assert.Equal(t, "cleanup-old-stuff", profile.Hooks.BeforeRemoveNode[0].Runbook.Name)
	})

	t.Run("prefers active file over deprecated when both exist", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "configProfiles", "deprecated"), 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "configProfiles", "my-profile.yaml"),
			[]byte(`hooks:
  beforeRemoveNode:
    - runbook:
        name: active-hook
`),
			0o644,
		))
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "configProfiles", "deprecated", "my-profile.yaml"),
			[]byte(`hooks:
  beforeRemoveNode:
    - runbook:
        name: deprecated-hook
`),
			0o644,
		))

		profile, missing, err := getConfigProfile(dir, "my-profile")

		require.NoError(t, err)
		assert.False(t, missing)
		require.Len(t, profile.Hooks.BeforeRemoveNode, 1)
		assert.Equal(t, "active-hook", profile.Hooks.BeforeRemoveNode[0].Runbook.Name)
	})

	t.Run("returns empty profile and missing=true when absent from both active and deprecated dirs", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "configProfiles", "deprecated"), 0o755))

		profile, missing, err := getConfigProfile(dir, "fully-retired-profile")

		require.NoError(t, err)
		assert.True(t, missing)
		assert.Nil(t, profile.Hooks.BeforeJoinNode)
		assert.Nil(t, profile.Hooks.AfterJoinNode)
		assert.Nil(t, profile.Hooks.BeforeRemoveNode)
		assert.Nil(t, profile.Hooks.AfterRemoveNode)
	})

	t.Run("returns empty profile when config profile name is omitted", func(t *testing.T) {
		dir := t.TempDir()

		profile, missing, err := getConfigProfile(dir, "")

		require.NoError(t, err)
		assert.False(t, missing)
		assert.Nil(t, profile.Hooks.BeforeJoinNode)
		assert.Nil(t, profile.Hooks.AfterJoinNode)
		assert.Nil(t, profile.Hooks.BeforeRemoveNode)
		assert.Nil(t, profile.Hooks.AfterRemoveNode)
	})

	t.Run("returns error when file exists but contains invalid YAML", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "configProfiles"), 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "configProfiles", "bad-profile.yaml"),
			[]byte("hooks: [\ninvalid yaml"),
			0o644,
		))

		_, _, err := getConfigProfile(dir, "bad-profile")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to unmarshal config profile")
	})

	t.Run("returns empty profile with no hooks when file is empty", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "configProfiles"), 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "configProfiles", "empty-profile.yaml"),
			[]byte(""),
			0o644,
		))

		profile, missing, err := getConfigProfile(dir, "empty-profile")

		require.NoError(t, err)
		assert.False(t, missing)
		assert.Nil(t, profile.Hooks.BeforeRemoveNode)
	})

	t.Run("returns empty profile with no hooks when file defines empty hooks", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "configProfiles"), 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "configProfiles", "retiring-profile.yaml"),
			[]byte("hooks: {}\n"),
			0o644,
		))

		profile, missing, err := getConfigProfile(dir, "retiring-profile")

		require.NoError(t, err)
		assert.False(t, missing)
		assert.Nil(t, profile.Hooks.BeforeRemoveNode)
		assert.Nil(t, profile.Hooks.AfterRemoveNode)
	})
}

// Config profiles and their lifecycle runbooks are one external runtime bundle,
// so validate the configured source when it is available.
func TestConfigProfileRunbookReferencesArePackaged(t *testing.T) {
	profilesDir := profilesDirForTest(t)
	runbooksDir := filepath.Join(filepath.Dir(profilesDir), "runbooks")

	require.NoError(t, runbook.ValidateProfileReferences(profilesDir, runbooksDir))
}

// profilesDirForTest resolves fleet profiles and their sibling runbooks.
// Order: NVKE_CONTROLLER_PROFILES_DIR (the deprecated NKE_CONTROLLER_PROFILES_DIR
// and CWE_PROFILES_DIR still work until #1399), sibling checkout, in-repo fixtures.
func profilesDirForTest(t *testing.T) string {
	t.Helper()
	if dir := cmp.Or(os.Getenv("NVKE_CONTROLLER_PROFILES_DIR"), os.Getenv("NKE_CONTROLLER_PROFILES_DIR"), os.Getenv("CWE_PROFILES_DIR")); dir != "" {
		return dir
	}
	sibling := filepath.Join(repoRoot(t), "..", "nke-site-deploy", "cwe-profiles", "0.1.0", "profiles")
	if _, err := os.Stat(sibling); err == nil {
		return sibling
	}
	// Fixtures last, so a real fleet tree wins when present.
	fixture := filepath.Join(repoRoot(t), "test", "fixtures", "profiles")
	if _, err := os.Stat(fixture); err == nil {
		return fixture
	}
	t.Skip("production profiles moved to nke-site-deploy; set NVKE_CONTROLLER_PROFILES_DIR to run")
	return ""
}

func repoRoot(t *testing.T) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", ".."))
}
