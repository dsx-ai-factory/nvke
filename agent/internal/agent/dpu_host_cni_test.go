// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDPUHostHelmProfileDoesNotLoadHostOVSDefaults(t *testing.T) {
	dir := t.TempDir()
	args := buildOvnKubernetesHelmInstallArgs(dir, "https://192.0.2.1:6443", "10.234.64.0/18", "10.234.0.0/18", "", "", map[string]string{ovnDpuHostConfigKey: "true"})
	require.Contains(t, args, filepath.Join(dir, "values-vmaas-nooverlay-dpu-host.yaml"))
	require.NotContains(t, args, filepath.Join(dir, "values-single-node-zone.yaml"))
	require.NotContains(t, args, "--namespace")
	require.NotContains(t, args, "--create-namespace")
	require.NotContains(t, strings.Join(args, " "), ovnDpuHostConfigKey+"=")
	require.False(t, ovnDpuHostMode(nil))
}
