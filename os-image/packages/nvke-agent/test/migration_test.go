// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func migrationScript(t *testing.T, root string) string {
	t.Helper()
	source, err := os.ReadFile("../../../../agent/scripts/migrate-agent-paths.sh")
	require.NoError(t, err)
	script := string(source)
	for _, prefix := range []string{"/var/lib", "/usr/local", "/etc"} {
		script = strings.ReplaceAll(script, prefix, filepath.Join(root, prefix))
	}
	path := filepath.Join(root, "migrate.sh")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func writeFixture(t *testing.T, root, path, content string) {
	t.Helper()
	path = filepath.Join(root, path)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func TestMigrateAgentPaths(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"var/lib/nke/state.json":                               "persistent node state",
		"var/lib/nke/enc-rotation.state":                       "rotation checkpoint",
		"var/lib/nke/agent-upgrades/op-1/operation.json":       "upgrade checkpoint",
		"usr/local/nke/user-scripts/check.sh":                  "user script",
		"etc/default/nke-agent":                                "AGENT_ARGS=--disable-mtls",
		"etc/nke/controller-ca.crt":                            "controller-owned placeholder",
		"etc/systemd/system/nke-agent.service.d/10-spire.conf": "spire ordering",
	}
	for path, content := range files {
		writeFixture(t, root, path, content)
	}
	script := migrationScript(t, root)
	for range 2 {
		output, err := exec.Command("bash", script).CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
	for old, content := range files {
		newPath := strings.ReplaceAll(old, "nke", "nvke")
		if strings.HasPrefix(old, "etc/nke/") {
			newPath = old // The controller owns this directory.
		}
		actual, err := os.ReadFile(filepath.Join(root, newPath))
		require.NoError(t, err)
		assert.Equal(t, content, string(actual))
		info, err := os.Stat(filepath.Join(root, newPath))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	// A legacy controller and an old rollback agent both still write the same files.
	writeFixture(t, root, "etc/default/nke-agent", "updated arguments")
	actual, err := os.ReadFile(filepath.Join(root, "etc/default/nvke-agent"))
	require.NoError(t, err)
	assert.Equal(t, "updated arguments", string(actual))
	alias, err := os.Readlink(filepath.Join(root, "var/lib/nke"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "var/lib/nvke"), alias)
}

func TestMigrationRefusesIndependentStateTrees(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"nke", "nvke"} {
		writeFixture(t, root, "var/lib/"+name+"/state.json", name)
	}
	_, err := exec.Command("bash", migrationScript(t, root)).CombinedOutput()
	require.Error(t, err)
	for _, name := range []string{"nke", "nvke"} {
		data, err := os.ReadFile(filepath.Join(root, "var/lib", name, "state.json"))
		require.NoError(t, err)
		assert.Equal(t, name, string(data))
	}
}

func TestFreshMigrationCreatesRollbackAliases(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "etc/default"), 0o755))
	output, err := exec.Command("bash", migrationScript(t, root)).CombinedOutput()
	require.NoError(t, err, "%s", output)
	writeFixture(t, root, "var/lib/nke/state.json", "rollback state")
	data, err := os.ReadFile(filepath.Join(root, "var/lib/nvke/state.json"))
	require.NoError(t, err)
	assert.Equal(t, "rollback state", string(data))
}
