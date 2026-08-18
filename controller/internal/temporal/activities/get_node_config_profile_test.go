// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/NVIDIA/nke/controller/internal/runbook"
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

// Config profiles reference runbooks by name, so validate that those names
// resolve to embedded scripts at test time instead of failing at runtime.
//
// Current profiles validate all hooks; deprecated profiles validate teardown
// hooks only, matching getConfigProfile's compatibility intent.
func TestConfigProfileRunbookReferencesAreEmbedded(t *testing.T) {
	configProfileRunbooks := referencedRunbooksFromConfigProfiles(t)
	for name := range configProfileRunbooks {
		_, err := runbook.Script(name)
		require.NoError(t, err, "config-profile referenced runbook %q is not embedded", name)
	}
}

func referencedRunbooksFromConfigProfiles(t *testing.T) map[string]struct{} {
	t.Helper()

	runbooks := map[string]struct{}{}
	configProfilesDir := configProfilesDirForTest(t)
	err := filepath.WalkDir(configProfilesDir, func(profilePath string, d os.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(profilePath) != ".yaml" {
			return nil
		}

		profileBytes, err := os.ReadFile(profilePath)
		require.NoError(t, err)

		var profile ConfigProfile
		require.NoError(t, yaml.Unmarshal(profileBytes, &profile), "failed to unmarshal %s", profilePath)

		relPath, err := filepath.Rel(configProfilesDir, profilePath)
		require.NoError(t, err)
		isDeprecated := strings.HasPrefix(relPath, "deprecated"+string(filepath.Separator))

		if !isDeprecated {
			for _, hook := range profile.Hooks.BeforeJoinNode {
				addHookRunbook(runbooks, hook)
			}
			for _, hook := range profile.Hooks.AfterJoinNode {
				addHookRunbook(runbooks, hook)
			}
		}

		// Deprecated profiles may still need to run teardown hooks.
		for _, hook := range profile.Hooks.BeforeRemoveNode {
			addHookRunbook(runbooks, hook)
		}
		for _, hook := range profile.Hooks.AfterRemoveNode {
			addHookRunbook(runbooks, hook)
		}

		return nil
	})
	require.NoError(t, err)

	return runbooks
}

func addHookRunbook(runbooks map[string]struct{}, hook *HookSpec) {
	if hook == nil || hook.Runbook == nil || hook.Runbook.Name == "" {
		return
	}
	runbooks[hook.Runbook.Name] = struct{}{}
}

// configProfilesDirForTest resolves fleet config profiles (NKX-13910 moved them
// to nke-site-deploy). Prefer CWE_PROFILES_DIR, then sibling checkout.
func configProfilesDirForTest(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("CWE_PROFILES_DIR"); dir != "" {
		return filepath.Join(dir, "configProfiles")
	}
	sibling := filepath.Join(repoRoot(t), "..", "nke-site-deploy", "cwe-profiles", "0.1.0", "profiles", "configProfiles")
	if _, err := os.Stat(sibling); err == nil {
		return sibling
	}
	t.Skip("production config profiles moved to nke-site-deploy; set CWE_PROFILES_DIR to run")
	return ""
}

func repoRoot(t *testing.T) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", ".."))
}
