// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agentupgrade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type bundleRenameRunner struct {
	bundleDir     string
	activeService string
	failTarget    bool
}

func (r *bundleRenameRunner) Run(_ context.Context, name string, args []string, _ string) ([]byte, error) {
	switch name {
	case "curl":
		return nil, os.WriteFile(valueAfter(args, "--output"), []byte("archive fixture"), 0o600)
	case "tar":
		entries, err := os.ReadDir(r.bundleDir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if err := copyRegularFile(filepath.Join(r.bundleDir, entry.Name()), filepath.Join(valueAfter(args, "-C"), entry.Name())); err != nil {
				return nil, err
			}
		}
		return nil, nil
	case "install":
		source, target := args[len(args)-2], args[len(args)-1]
		data, err := os.ReadFile(source)
		if err != nil {
			return nil, err
		}
		if r.failTarget && strings.Contains(string(data), "v0.3.0") {
			r.failTarget = false
			return nil, errors.New("target replacement failed")
		}
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, copyRegularFile(source, target)
	case "bash":
		return nil, nil // The shared migration has its own filesystem tests.
	case "systemctl":
		switch args[0] {
		case "restart":
			r.activeService = args[1]
		case "is-active":
			if args[len(args)-1] != r.activeService {
				return nil, errors.New("service not active")
			}
		case "stop":
			r.activeService = ""
		}
		return nil, nil
	default:
		if len(args) == 1 && args[0] == "--version" {
			return os.ReadFile(name)
		}
		return nil, fmt.Errorf("unexpected command %s %v", name, args)
	}
}

func writeBundleLayout(t *testing.T, binDir, unitDir, name, version string) {
	t.Helper()
	for _, dir := range []string{binDir, unitDir} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	prefix := "NVKE Agent Version: "
	if name == legacyAgentPackageName {
		prefix = legacyAgentVersionPrefix + " "
	}
	require.NoError(t, os.WriteFile(filepath.Join(binDir, name), []byte(prefix+version), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(binDir, name+"-updater"), []byte("updater"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(unitDir, name+".service"), []byte("agent unit"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(unitDir, name+"-updater@.service"), []byte("updater unit"), 0o644))
	if name == agentPackageName {
		require.NoError(t, os.WriteFile(filepath.Join(binDir, "migrate-agent-paths.sh"), []byte("migration fixture"), 0o755))
	}
}

func TestBundleRenameUpgradeAndRestore(t *testing.T) {
	for _, previous := range []string{agentPackageName, legacyAgentPackageName} {
		for _, target := range []string{agentPackageName, legacyAgentPackageName} {
			for _, failTarget := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s_to_%s_failure_%t", previous, target, failTarget), func(t *testing.T) {
					root := t.TempDir()
					binDir, unitDir := filepath.Join(root, "bin"), filepath.Join(root, "units")
					writeBundleLayout(t, binDir, unitDir, previous, "v0.2.0")
					bundleDir := filepath.Join(root, "bundle")
					writeBundleLayout(t, bundleDir, bundleDir, target, "v0.3.0")
					store := NewStore(filepath.Join(root, "operations"))
					_, _, err := store.Create(Operation{Method: MethodBundle, TargetVersion: "v0.3.0", PreviousVersion: "v0.2.0", ArtifactURL: "https://example.test/agent.tgz"})
					require.NoError(t, err)
					runner := &bundleRenameRunner{bundleDir: bundleDir, activeService: previous + ".service", failTarget: failTarget}
					updater := NewUpdater(store)
					updater.Runner, updater.AgentBinary, updater.UpdaterBinary, updater.SystemdUnitDir = runner, filepath.Join(binDir, agentPackageName), filepath.Join(binDir, agentPackageName+"-updater"), unitDir
					updater.MigrationScript = filepath.Join(binDir, "migrate-agent-paths.sh")
					updater.VerifyAttempts = 1
					err = updater.Run(context.Background(), "v0.3.0_bundle")
					op, loadErr := store.Load("v0.3.0_bundle")
					require.NoError(t, loadErr)
					if failTarget {
						require.Error(t, err)
						assert.Equal(t, PhaseFailedRestored, op.Phase)
						assert.Equal(t, previous+".service", runner.activeService)
					} else {
						require.NoError(t, err)
						assert.Equal(t, PhaseSucceeded, op.Phase)
						assert.Equal(t, target+".service", runner.activeService)
						// A downgrade installs the older artifact through the same request path.
						downgradeDir := filepath.Join(root, "downgrade")
						writeBundleLayout(t, downgradeDir, downgradeDir, previous, "v0.2.0")
						runner.bundleDir = downgradeDir
						_, _, err = store.Create(Operation{TargetVersion: "v0.2.0", PreviousVersion: "v0.3.0", Method: MethodBundle, ArtifactURL: "https://example.test/older-agent.tgz"})
						require.NoError(t, err)
						require.NoError(t, updater.Run(context.Background(), "v0.2.0_bundle"))
						assert.Equal(t, previous+".service", runner.activeService)
					}
					data, err := os.ReadFile(filepath.Join(binDir, previous))
					require.NoError(t, err)
					assert.Equal(t, "v0.2.0", parseAgentVersion(string(data)))
				})
			}
		}
	}
}

func TestBundleRenameResumeRetainsOriginalLayout(t *testing.T) {
	for _, legacySnapshot := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy_snapshot_%t", legacySnapshot), func(t *testing.T) {
			root := t.TempDir()
			binDir, unitDir := filepath.Join(root, "bin"), filepath.Join(root, "units")
			writeBundleLayout(t, binDir, unitDir, legacyAgentPackageName, "v0.2.0")
			bundleDir := filepath.Join(root, "bundle")
			writeBundleLayout(t, bundleDir, bundleDir, agentPackageName, "v0.3.0")
			store := NewStore(filepath.Join(root, "operations"))
			_, _, err := store.Create(Operation{Method: MethodBundle, TargetVersion: "v0.3.0", PreviousVersion: "v0.2.0", ArtifactURL: "https://example.test/agent.tgz"})
			require.NoError(t, err)
			runner := &bundleRenameRunner{bundleDir: bundleDir, activeService: legacyAgentPackageName + ".service"}
			newUpdater := func() *Updater {
				updater := NewUpdater(store)
				updater.Runner, updater.AgentBinary, updater.UpdaterBinary, updater.SystemdUnitDir = runner, filepath.Join(binDir, agentPackageName), filepath.Join(binDir, agentPackageName+"-updater"), unitDir
				updater.MigrationScript, updater.VerifyAttempts = filepath.Join(binDir, "migrate-agent-paths.sh"), 1
				return updater
			}
			first := newUpdater()
			require.NoError(t, first.snapshotInstalledFiles("v0.3.0_bundle"))
			if legacySnapshot {
				assets, err := first.assetsDir("v0.3.0_bundle")
				require.NoError(t, err)
				require.NoError(t, os.Remove(filepath.Join(assets, "previous", ".snapshot-complete")))
			}
			require.NoError(t, first.installBundleFiles(context.Background(), bundleDir))
			// Simulate a process interruption after replacement and a failed retry.
			runner.failTarget = true
			require.Error(t, newUpdater().Run(context.Background(), "v0.3.0_bundle"))
			op, err := store.Load("v0.3.0_bundle")
			require.NoError(t, err)
			assert.Equal(t, PhaseFailedRestored, op.Phase)
			assert.Equal(t, legacyAgentPackageName+".service", runner.activeService)
			data, err := os.ReadFile(filepath.Join(binDir, legacyAgentPackageName))
			require.NoError(t, err)
			assert.Equal(t, "v0.2.0", parseAgentVersion(string(data)))
		})
	}
}
