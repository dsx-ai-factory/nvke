// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agentupgrade

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

type bundleInstallRunner struct{}

func (bundleInstallRunner) Run(_ context.Context, name string, args []string, _ string) ([]byte, error) {
	if name == "install" {
		return nil, copyRegularFile(args[len(args)-2], args[len(args)-1])
	}
	return nil, nil
}

func TestBundleDowngradePreservesUpdaterAndOriginalRestorationFiles(t *testing.T) {
	root := t.TempDir()
	updater := NewUpdater(NewStore(root))
	updater.AgentBinary = filepath.Join(root, "nvke-agent")
	updater.UpdaterBinary = filepath.Join(root, "nvke-agent-updater")
	updater.SystemdUnitDir = root
	updater.MigrationScript = filepath.Join(root, "migrate-agent-paths.sh")
	updater.Runner = bundleInstallRunner{}
	for _, name := range []string{"nvke-agent", "nvke-agent-updater", "nvke-agent.service", "nvke-agent-updater@.service", "migrate-agent-paths.sh"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("original"), 0o600))
	}
	_, _, err := updater.Store.Create(Operation{TargetVersion: "v0.1.1", PreviousVersion: "v0.1.2", Method: MethodBundle})
	require.NoError(t, err)
	require.NoError(t, updater.snapshotInstalledFiles("v0.1.1_bundle"))
	extracted := t.TempDir()
	for _, name := range []string{"nvke-agent", "nvke-agent.service", "migrate-agent-paths.sh"} {
		require.NoError(t, os.WriteFile(filepath.Join(extracted, name), []byte("older release"), 0o600))
	}
	require.NoError(t, updater.installBundleFiles(context.Background(), extracted))
	require.NoError(t, updater.snapshotInstalledFiles("v0.1.1_bundle"))
	originalUpdater, err := os.ReadFile(updater.UpdaterBinary)
	require.NoError(t, err)
	require.Equal(t, "original", string(originalUpdater))
	assets, err := updater.assetsDir("v0.1.1_bundle")
	require.NoError(t, err)
	retained, err := os.ReadFile(filepath.Join(assets, "previous", "nvke-agent"))
	require.NoError(t, err)
	require.Equal(t, "original", string(retained))
}

func TestBundleSnapshotPublishesOnlyCompleteBackups(t *testing.T) {
	root := t.TempDir()
	store := NewStore(filepath.Join(root, "operations"))
	_, _, err := store.Create(Operation{TargetVersion: "v0.1.2", PreviousVersion: "v0.1.1", Method: MethodBundle})
	require.NoError(t, err)
	updater := NewUpdater(store)
	updater.AgentBinary = filepath.Join(root, "nvke-agent")
	updater.UpdaterBinary = filepath.Join(root, "nvke-agent-updater")
	updater.SystemdUnitDir = filepath.Join(root, "units")
	updater.MigrationScript = filepath.Join(root, "migrate-agent-paths.sh")
	require.NoError(t, os.MkdirAll(updater.SystemdUnitDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(updater.SystemdUnitDir, agentServiceName), []byte("original"), 0o600))
	// A nonregular source must never publish a backup that a retry skips.
	require.NoError(t, os.Mkdir(updater.AgentBinary, 0o700))
	require.ErrorContains(t, updater.snapshotInstalledFiles("v0.1.2_bundle"), "not a regular file")
	assets, err := updater.assetsDir("v0.1.2_bundle")
	require.NoError(t, err)
	backup := filepath.Join(assets, "previous")
	entries, err := os.ReadDir(backup)
	require.NoError(t, err)
	require.Empty(t, entries)
	require.NoError(t, os.Remove(updater.AgentBinary))
	for _, source := range []string{updater.AgentBinary, updater.UpdaterBinary, filepath.Join(updater.SystemdUnitDir, agentServiceName), filepath.Join(updater.SystemdUnitDir, "nvke-agent-updater@.service"), updater.MigrationScript} {
		require.NoError(t, os.WriteFile(source, []byte("original"), 0o700))
	}
	// An interrupted unpublished copy must not become the restoration source.
	require.NoError(t, os.WriteFile(filepath.Join(backup, ".backup-interrupted"), []byte("partial"), 0o600))
	require.NoError(t, updater.snapshotInstalledFiles("v0.1.2_bundle"))
	require.NoError(t, os.WriteFile(updater.AgentBinary, []byte("replacement"), 0o700))
	require.NoError(t, updater.snapshotInstalledFiles("v0.1.2_bundle"))
	retained, err := os.ReadFile(filepath.Join(backup, "nvke-agent"))
	require.NoError(t, err)
	require.Equal(t, "original", string(retained))
}
