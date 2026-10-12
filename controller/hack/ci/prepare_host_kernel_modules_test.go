// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ci

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostKernelPackageVersion(t *testing.T) {
	tests := []struct {
		name      string
		signature string
		release   string
		want      string
		wantError bool
	}{
		{
			name:      "Jammy AWS backport retains its distribution build",
			signature: "Ubuntu 6.8.0-1051.54~22.04.1-aws 6.8.12",
			release:   "6.8.0-1051-aws",
			want:      "6.8.0-1051.54~22.04.1\n",
		},
		{
			name:      "Noble AWS build with the same release is distinct",
			signature: "Ubuntu 6.8.0-1051.54-aws 6.8.12",
			release:   "6.8.0-1051-aws",
			want:      "6.8.0-1051.54\n",
		},
		{
			name:      "native generic build",
			signature: "Ubuntu 6.8.0-117.117-generic 6.8.12",
			release:   "6.8.0-117-generic",
			want:      "6.8.0-117.117\n",
		},
		{
			name:      "multi-part AWS flavor",
			signature: "Ubuntu 6.8.0-1051.54~22.04.1-aws-64k 6.8.12",
			release:   "6.8.0-1051-aws-64k",
			want:      "6.8.0-1051.54~22.04.1\n",
		},
		{
			name:      "mismatched ABI fails closed",
			signature: "Ubuntu 6.8.0-1051.54~22.04.1-aws 6.8.12",
			release:   "6.8.0-1052-aws",
			wantError: true,
		},
		{
			name:      "unidentified build fails closed",
			signature: "Linux 6.8.0-1051-aws",
			release:   "6.8.0-1051-aws",
			wantError: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "bash", "-c",
				`source ./prepare-host-kernel-modules.sh; host_kernel_package_version "$1" "$2"`,
				"--", tt.signature, tt.release)
			output, err := cmd.CombinedOutput()
			if tt.wantError {
				require.Error(t, err, string(output))
				return
			}
			require.NoError(t, err, string(output))
			assert.Equal(t, tt.want, string(output))
		})
	}
}

func TestHostKernelArchiveSuite(t *testing.T) {
	tests := []struct {
		name      string
		version   string
		runner    string
		want      string
		wantError bool
	}{
		{name: "Jammy host on Noble runner", version: "6.8.0-1051.54~22.04.1", runner: "noble", want: "jammy\n"},
		{name: "Noble backport", version: "6.17.0-1012.12~24.04.1", runner: "noble", want: "noble\n"},
		{name: "native build", version: "6.8.0-1051.54", runner: "noble", want: "noble\n"},
		{name: "unrecognized backport fails closed", version: "6.8.0-1051.54~custom.1", runner: "noble", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "bash", "-c",
				`source ./prepare-host-kernel-modules.sh; host_kernel_archive_suite "$1" "$2"`,
				"--", tt.version, tt.runner)
			output, err := cmd.CombinedOutput()
			if tt.wantError {
				require.Error(t, err, string(output))
				return
			}
			require.NoError(t, err, string(output))
			assert.Equal(t, tt.want, string(output))
		})
	}
}
