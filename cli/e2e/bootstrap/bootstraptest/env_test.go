// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cliDirWithNvkectl returns a stand-in cli directory holding bin/nvkectl, so
// FindNvkectl resolves without NVKECTL being set.
func cliDirWithNvkectl(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin", "nvkectl"), []byte("#!/bin/sh\n"), 0o755))
	return dir
}

// setLaneEnv clears every variable the lane configuration reads, then applies
// env. Clearing first keeps a variable set in the developer's shell out of the
// case.
func setLaneEnv(t *testing.T, env map[string]string) {
	t.Helper()
	all := []string{
		"NVKECTL", EnvSiteSpec, EnvBootstrapLog, EnvKubeconfigOut, EnvEncryptionKeyOut,
		EnvDiagnosticsDir, EnvTimeout, EnvKeepBootstrapCluster, EnvDisableMTLS,
		EnvVerbose,
	}
	for _, name := range all {
		t.Setenv(name, "")
	}
	for name, value := range env {
		t.Setenv(name, value)
	}
}

func TestBootstrapOptionsFromEnvReadsTheProvisionedLane(t *testing.T) {
	cliDir := cliDirWithNvkectl(t)
	setLaneEnv(t, map[string]string{
		EnvSiteSpec:             "/tmp/site.yaml",
		EnvBootstrapLog:         "/tmp/bootstrap.log",
		EnvKubeconfigOut:        "/tmp/kubeconfig",
		EnvEncryptionKeyOut:     "/tmp/etcd.key",
		EnvDiagnosticsDir:       "/tmp/diag",
		EnvTimeout:              "15m",
		EnvKeepBootstrapCluster: "1",
		EnvVerbose:              "true",
	})

	opts, err := BootstrapOptionsFromEnv(cliDir)
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(cliDir, "bin", "nvkectl"), opts.Binary)
	assert.Equal(t, "/tmp/site.yaml", opts.SiteSpec)
	assert.Equal(t, "/tmp/bootstrap.log", opts.LogFile)
	assert.Equal(t, "/tmp/kubeconfig", opts.KubeconfigOut)
	assert.Equal(t, "/tmp/etcd.key", opts.EncryptionKeyOut)
	assert.Equal(t, "/tmp/diag", opts.DiagnosticsDir)
	assert.Equal(t, 15*time.Minute, opts.Timeout)
	assert.True(t, opts.KeepBootstrapCluster)
	assert.True(t, opts.Verbose)
	assert.False(t, opts.DisableMTLS)
}

func TestBootstrapOptionsFromEnvDefaults(t *testing.T) {
	cliDir := cliDirWithNvkectl(t)
	setLaneEnv(t, map[string]string{EnvSiteSpec: "/tmp/site.yaml"})

	opts, err := BootstrapOptionsFromEnv(cliDir)
	require.NoError(t, err)

	// Nothing the lane did not ask for.
	assert.Equal(t, []string{"bootstrap", "site", "-f", "/tmp/site.yaml"}, opts.args())
	assert.Empty(t, opts.LogFile)
	assert.Equal(t, DefaultStopGrace, opts.stopGrace())
}

func TestBootstrapOptionsFromEnvRejectsBadValues(t *testing.T) {
	cliDir := cliDirWithNvkectl(t)

	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name:    "no site spec",
			env:     map[string]string{},
			wantErr: EnvSiteSpec + " is not set",
		},
		{
			name:    "a timeout that is not a duration",
			env:     map[string]string{EnvSiteSpec: "/tmp/site.yaml", EnvTimeout: "15"},
			wantErr: "is not a duration",
		},
		{
			name:    "a negative timeout",
			env:     map[string]string{EnvSiteSpec: "/tmp/site.yaml", EnvTimeout: "-5m"},
			wantErr: "is negative",
		},
		{
			name:    "a keep-cluster flag that is not a boolean",
			env:     map[string]string{EnvSiteSpec: "/tmp/site.yaml", EnvKeepBootstrapCluster: "yes please"},
			wantErr: "is not a boolean",
		},
		{
			name:    "a verbose flag that is not a boolean",
			env:     map[string]string{EnvSiteSpec: "/tmp/site.yaml", EnvVerbose: "loud"},
			wantErr: "is not a boolean",
		},
		{
			name:    "a disable-mTLS flag that is not a boolean",
			env:     map[string]string{EnvSiteSpec: "/tmp/site.yaml", EnvDisableMTLS: "maybe"},
			wantErr: "is not a boolean",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setLaneEnv(t, tt.env)
			_, err := BootstrapOptionsFromEnv(cliDir)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestBootstrapOptionsFromEnvReportsAMissingNvkectl(t *testing.T) {
	setLaneEnv(t, map[string]string{EnvSiteSpec: "/tmp/site.yaml"})
	_, err := BootstrapOptionsFromEnv(t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not there")
}

func TestBoolEnv(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{raw: "", want: false},
		{raw: "1", want: true},
		{raw: "0", want: false},
		{raw: "true", want: true},
		{raw: "false", want: false},
	}

	for _, tt := range tests {
		t.Run("value "+tt.raw, func(t *testing.T) {
			t.Setenv("NVKECTL_E2E_TEST_BOOL", tt.raw)
			got, err := boolEnv("NVKECTL_E2E_TEST_BOOL")
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
