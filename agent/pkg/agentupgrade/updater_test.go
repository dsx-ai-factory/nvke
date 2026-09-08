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
	currentVersion    string
	currentDebVersion string
	failTargetInstall bool
	installedVersions []string
}

func (r *aptUpdaterRunner) Run(_ context.Context, name string, args []string, _ string) ([]byte, error) {
	switch name {
	case "apt-get":
		if containsArg(args, "--download-only") {
			version := strings.TrimPrefix(args[len(args)-1], agentPackageName+"=")
			assetsDir := strings.TrimPrefix(valueAfter(args, "-o"), "Dir::Cache::archives=")
			path := filepath.Join(assetsDir, fmt.Sprintf("nke-agent_%s_native.deb", version))
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
			r.currentVersion = "v" + version
			r.currentDebVersion = version
			return nil, nil
		}
		return nil, nil
	case "dpkg-query":
		return []byte(r.currentDebVersion), nil
	case "dpkg-deb":
		if args[len(args)-1] == "Package" {
			return []byte(agentPackageName), nil
		}
		return []byte(debVersionFromPath(args[len(args)-2])), nil
	case "/usr/local/bin/nke-agent":
		return []byte("NKE Agent Version: " + r.currentVersion + "\n"), nil
	case "systemctl":
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected command %s %v", name, args)
	}
}

func TestUpdaterAPTUpgradeAndAutomaticRestore(t *testing.T) {
	tests := []struct {
		name              string
		failTargetInstall bool
		wantPhase         Phase
		wantInstalled     []string
		wantError         bool
	}{
		{
			name:          "exact target succeeds",
			wantPhase:     PhaseSucceeded,
			wantInstalled: []string{"0.1.2"},
		},
		{
			name:              "failed target restores exact previous package",
			failTargetInstall: true,
			wantPhase:         PhaseFailedRestored,
			wantInstalled:     []string{"0.1.1"},
			wantError:         true,
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
	withoutPackage := strings.TrimPrefix(base, "nke-agent_")
	architectureSeparator := strings.LastIndex(withoutPackage, "_")
	if architectureSeparator < 0 {
		return ""
	}
	return withoutPackage[:architectureSeparator]
}
