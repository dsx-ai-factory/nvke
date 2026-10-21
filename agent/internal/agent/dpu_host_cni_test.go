// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDPUHostHelmProfileDoesNotLoadHostOVSDefaults(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "values-single-node-zone.yaml"), nil, 0o600))
	args := buildOvnKubernetesHelmInstallArgs(dir, "https://192.0.2.1:6443", "10.234.64.0/18", "10.234.0.0/18", "", "", map[string]string{ovnDpuHostConfigKey: "true"})
	require.Contains(t, args, "tags.ovnkube-node-dpu-host=true")
	require.Contains(t, args, "tags.ovnkube-single-node-zone=false")
	require.Contains(t, args, "tags.ovs-node=false")
	require.Contains(t, args, filepath.Join(dir, "values-single-node-zone.yaml"))
	require.NotContains(t, args, "--namespace")
	require.NotContains(t, args, "--create-namespace")
	require.NotContains(t, strings.Join(args, " "), ovnDpuHostConfigKey+"=")
	require.False(t, ovnDpuHostMode(nil))
}
