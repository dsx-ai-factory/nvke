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

type aptUpdaterRunner struct {
	currentPackage    string
	targetPackage     string
	currentVersion    string
	currentDebVersion string
	failTargetInstall bool
	installedVersions []string
}

func (r *aptUpdaterRunner) Run(_ context.Context, name string, args []string, _ string) ([]byte, error) {
	switch name {
	case "apt-get":
		if containsArg(args, "--download-only") {
			pkg, version, _ := strings.Cut(args[len(args)-1], "=")
			assetsDir := strings.TrimPrefix(valueAfter(args, "-o"), "Dir::Cache::archives=")
			path := filepath.Join(assetsDir, fmt.Sprintf("%s_%s_native.deb", pkg, version))
			if err := os.WriteFile(path, []byte("test package"), 0o600); err != nil {
				return nil, err
			}
			return nil, nil
		}
		if containsArg(args, "install") && strings.HasSuffix(args[len(args)-1], ".deb") {
			version := debVersionFromPath(args[len(args)-1])
			if r.failTargetInstall && version == "0.1.2" {
				return nil, errors.New("target install failed")
			}
			r.installedVersions = append(r.installedVersions, version)
			r.currentPackage = packageFromDebPath(args[len(args)-1])
			r.currentVersion = "v" + version
			r.currentDebVersion = version
			return nil, nil
		}
		return nil, nil
	case "apt-cache":
		pkg, _, _ := strings.Cut(args[len(args)-1], "=")
		if pkg != r.targetPackage {
			return nil, errors.New("package unavailable")
		}
		return []byte("Package: " + pkg + "\n"), nil
	case "dpkg-query":
		if args[len(args)-1] != r.currentPackage {
			// dpkg-query can succeed for removed packages with retained config.
			return []byte("config-files 0.1.0"), nil
		}
		return []byte("installed " + r.currentDebVersion), nil
	case "dpkg-deb":
		if args[len(args)-1] == "Package" {
			return []byte(packageFromDebPath(args[len(args)-2])), nil
		}
		return []byte(debVersionFromPath(args[len(args)-2])), nil
	case "/usr/local/bin/nvke-agent", "/usr/local/bin/nke-agent":
		if filepath.Base(name) != r.currentPackage {
			return nil, errors.New("wrong agent binary")
		}
		prefix := "NVKE Agent Version: "
		if r.currentPackage == legacyAgentPackageName {
			prefix = "NKE Agent Version: "
		}
		return []byte(prefix + r.currentVersion + "\n"), nil
	case "systemctl":
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected command %s %v", name, args)
	}
}

func TestUpdaterAPTUpgradeAndAutomaticRestore(t *testing.T) {
	tests := []struct {
		name              string
		previousPackage   string
		targetPackage     string
		failTargetInstall bool
		wantPhase         Phase
		wantInstalled     []string
		wantError         bool
	}{
		{
			name:            "exact target succeeds",
			previousPackage: agentPackageName,
			targetPackage:   agentPackageName,
			wantPhase:       PhaseSucceeded,
			wantInstalled:   []string{"0.1.2"},
		},
		{
			name:              "failed target restores exact previous package",
			previousPackage:   agentPackageName,
			targetPackage:     agentPackageName,
			failTargetInstall: true,
			wantPhase:         PhaseFailedRestored,
			wantInstalled:     []string{"0.1.1"},
			wantError:         true,
		},
		{
			name:            "legacy package upgrades to renamed package",
			previousPackage: legacyAgentPackageName,
			targetPackage:   agentPackageName,
			wantPhase:       PhaseSucceeded,
			wantInstalled:   []string{"0.1.2"},
		},
		{
			name:              "failed renamed package restores legacy package",
			previousPackage:   legacyAgentPackageName,
			targetPackage:     agentPackageName,
			failTargetInstall: true,
			wantPhase:         PhaseFailedRestored,
			wantInstalled:     []string{"0.1.1"},
			wantError:         true,
		},
		{
			name:            "renamed agent can install a legacy release",
			previousPackage: agentPackageName,
			targetPackage:   legacyAgentPackageName,
			wantPhase:       PhaseSucceeded,
			wantInstalled:   []string{"0.1.2"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := NewStore(t.TempDir())
			_, _, err := store.Create(Operation{
				ID:              "upgrade-1",
				Action:          ActionUpgrade,
				TargetVersion:   "v0.1.2",
				PreviousVersion: "v0.1.1",
				Method:          MethodAPT,
			})
			require.NoError(t, err)
			runner := &aptUpdaterRunner{
				currentPackage:    test.previousPackage,
				targetPackage:     test.targetPackage,
				currentVersion:    "v0.1.1",
				currentDebVersion: "0.1.1",
				failTargetInstall: test.failTargetInstall,
			}
			updater := NewUpdater(store)
			updater.Runner = runner
			updater.VerifyAttempts = 1

			err = updater.Run(context.Background(), "upgrade-1")
			if test.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			operation, loadErr := store.Load("upgrade-1")
			require.NoError(t, loadErr)
			assert.Equal(t, test.wantPhase, operation.Phase)
			assert.Equal(t, "0.1.1", operation.PreviousPackageVersion)
			assert.Equal(t, test.previousPackage, operation.PreviousPackageName)
			assert.Equal(t, test.wantInstalled, runner.installedVersions)
		})
	}
}

func containsArg(args []string, value string) bool {
	for _, arg := range args {
		if arg == value {
			return true
		}
	}
	return false
}

func valueAfter(args []string, name string) string {
	for index, arg := range args {
		if arg == name && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

func debVersionFromPath(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".deb")
	withoutPackage := strings.TrimPrefix(base, packageFromDebPath(path)+"_")
	architectureSeparator := strings.LastIndex(withoutPackage, "_")
	if architectureSeparator < 0 {
		return ""
	}
	return withoutPackage[:architectureSeparator]
}

func packageFromDebPath(path string) string {
	name, _, _ := strings.Cut(filepath.Base(path), "_")
	return name
}

func TestParseAgentVersion(t *testing.T) {
	for _, prefix := range []string{"NVKE Agent Version:", "NKE Agent Version:"} {
		assert.Equal(t, "v0.3.0", parseAgentVersion(prefix+" v0.3.0\n"))
	}
	assert.Empty(t, parseAgentVersion("NVKE Agent Updater Version: v0.3.0"))
}

func TestAPTRollbackPreservesPackageNameAtSameVersion(t *testing.T) {
	store := NewStore(t.TempDir())
	_, _, err := store.Create(Operation{ID: "rename", Action: ActionUpgrade, Method: MethodAPT, TargetVersion: "v0.1.1", PreviousVersion: "v0.1.1"})
	require.NoError(t, err)
	runner := &aptUpdaterRunner{currentPackage: legacyAgentPackageName, targetPackage: agentPackageName, currentVersion: "v0.1.1", currentDebVersion: "0.1.1"}
	updater := NewUpdater(store)
	updater.Runner, updater.VerifyAttempts = runner, 1
	require.NoError(t, updater.Run(context.Background(), "rename"))
	assert.Equal(t, agentPackageName, runner.currentPackage)
	_, _, err = store.Create(Operation{ID: "restore", Action: ActionRollback, RestoreFromOperationID: "rename"})
	require.NoError(t, err)
	require.NoError(t, updater.Run(context.Background(), "restore"))
	assert.Equal(t, legacyAgentPackageName, runner.currentPackage)
	assert.Equal(t, "0.1.1", runner.currentDebVersion)
}

func TestAPTResumeKeepsOriginalRollbackPackage(t *testing.T) {
	store := NewStore(t.TempDir())
	_, _, err := store.Create(Operation{ID: "resume", Action: ActionUpgrade, Method: MethodAPT, TargetVersion: "v0.1.2", PreviousVersion: "v0.1.1"})
	require.NoError(t, err)
	_, err = store.SetPreviousPackage("resume", legacyAgentPackageName, "0.1.1")
	require.NoError(t, err)
	// The renamed target is installed when a prior updater process is interrupted.
	runner := &aptUpdaterRunner{currentPackage: agentPackageName, targetPackage: agentPackageName, currentVersion: "v0.1.2", currentDebVersion: "0.1.2", failTargetInstall: true}
	updater := NewUpdater(store)
	updater.Runner, updater.VerifyAttempts = runner, 1
	require.Error(t, updater.Run(context.Background(), "resume"))
	op, err := store.Load("resume")
	require.NoError(t, err)
	assert.Equal(t, PhaseFailedRestored, op.Phase)
	assert.Equal(t, legacyAgentPackageName, op.PreviousPackageName)
	assert.Equal(t, "0.1.1", op.PreviousPackageVersion)
	assert.Equal(t, legacyAgentPackageName, runner.currentPackage)
}
