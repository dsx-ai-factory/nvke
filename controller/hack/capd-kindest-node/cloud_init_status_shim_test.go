// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capdkindestnode

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCloudInitStatusShim(t *testing.T) {
	tests := []struct {
		name  string
		state *string
		want  string
	}{
		{name: "missing state is running", want: "status: running\n"},
		{name: "running", state: stringPtr("running\n"), want: "status: running\n"},
		{name: "done", state: stringPtr("done\n"), want: "status: done\n"},
		{name: "error", state: stringPtr("error\n"), want: "status: error\n"},
		{name: "corrupt state fails closed", state: stringPtr("unknown\n"), want: "status: error\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := t.TempDir()
			statusFile := filepath.Join(tempDir, "status")
			if tt.state != nil {
				require.NoError(t, os.WriteFile(statusFile, []byte(*tt.state), 0o600))
			}

			cmd := exec.CommandContext(t.Context(), "sh", "scripts/cloud-init-status-shim", "status")
			cmd.Env = append(os.Environ(),
				"NICO_MOCK_CLOUD_INIT_STATUS_FILE="+statusFile,
			)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, string(output))
			assert.Equal(t, tt.want, string(output))
		})
	}
}

func TestCloudInitStatusShimRejectsUnsupportedCommand(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "sh", "scripts/cloud-init-status-shim", "modules")
	output, err := cmd.CombinedOutput()
	require.Error(t, err)
	assert.Contains(t, string(output), "only 'cloud-init status' is supported")
}

func stringPtr(value string) *string {
	return &value
}
