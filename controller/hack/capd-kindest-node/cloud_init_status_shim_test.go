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
		{name: "missing state is running", want: "running"},
		{name: "running", state: stringPtr("running\n"), want: "running"},
		{name: "done", state: stringPtr("done\n"), want: "done"},
		{name: "error", state: stringPtr("error\n"), want: "error"},
		{name: "corrupt state fails closed", state: stringPtr("unknown\n"), want: "error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			statusFile := filepath.Join(t.TempDir(), "status")
			if tt.state != nil {
				require.NoError(t, os.WriteFile(statusFile, []byte(*tt.state), 0o600))
			}

			assert.Equal(t, "status: "+tt.want+"\n", runShim(t, statusFile, "status"))

			// The node agent asks for --long. The status line still comes
			// first, since that is the line the agent reads the state from.
			assert.Equal(t,
				"status: "+tt.want+"\n"+
					"extended_status: "+tt.want+"\n"+
					"boot_status_code: enabled-by-generator\n"+
					"detail: nico-containers cloud-init shim; no stage detail is simulated\n",
				runShim(t, statusFile, "status", "--long"))
		})
	}
}

func TestCloudInitStatusShimRejectsUnsupportedCommand(t *testing.T) {
	for name, args := range map[string][]string{
		"another subcommand": {"modules"},
		"no subcommand":      {},
		"another flag":       {"status", "--wait"},
		"too many arguments": {"status", "--long", "--wait"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "sh", append([]string{"scripts/cloud-init-status-shim"}, args...)...)
			output, err := cmd.CombinedOutput()
			require.Error(t, err)
			assert.Contains(t, string(output), "only 'cloud-init status [--long]' is supported")
		})
	}
}

func runShim(t *testing.T, statusFile string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sh", append([]string{"scripts/cloud-init-status-shim"}, args...)...)
	cmd.Env = append(os.Environ(), "NICO_CONTAINERS_CLOUD_INIT_STATUS_FILE="+statusFile)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	return string(output)
}

func stringPtr(value string) *string {
	return &value
}
