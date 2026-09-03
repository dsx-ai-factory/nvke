// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runbook

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScript(t *testing.T) {
	script, err := Script("", "reboot")

	require.NoError(t, err)
	require.Contains(t, script, "Rebooting the system now")
}

func TestScriptPrefersExternalRunbook(t *testing.T) {
	runbooksDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(runbooksDir, "reboot.sh"), []byte("external reboot\n"), 0o644))

	script, err := Script(runbooksDir, "reboot")

	require.NoError(t, err)
	require.Equal(t, "external reboot\n", script)
}

func TestScriptFallsBackToEmbeddedRunbook(t *testing.T) {
	script, err := Script(t.TempDir(), "reboot")

	require.NoError(t, err)
	require.Contains(t, script, "Rebooting the system now")
}

func TestScriptRejectsInvalidName(t *testing.T) {
	_, err := Script(t.TempDir(), "../reboot")

	require.EqualError(t, err, `invalid runbook name "../reboot"`)
}

func TestScriptReportsMissingRunbook(t *testing.T) {
	_, err := Script(t.TempDir(), "missing-runbook")

	require.ErrorContains(t, err, `runbook "missing-runbook" not found`)
}

func TestNames(t *testing.T) {
	names := Names()

	require.NotEmpty(t, names)
	require.Contains(t, names, "reboot")
	require.Contains(t, names, "uptime")
	require.Contains(t, names, "kubeadm-patches")
	require.Contains(t, names, "etcd-member-remove")
	require.True(t, sort.StringsAreSorted(names))
}

func TestKubeadmPatchesScriptWritesConfiguredMemoryReservations(t *testing.T) {
	tempDir := t.TempDir()
	script, err := Script("", "kubeadm-patches")
	require.NoError(t, err)
	scriptPath := filepath.Join(tempDir, "kubeadm-patches.sh")
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o700))

	cmd := exec.CommandContext(t.Context(), "bash", scriptPath)
	cmd.Env = append(os.Environ(),
		"KUBEADM_PATCHES_DIR="+tempDir,
		"KUBE_RESERVED_MEM=4Gi",
		"SYSTEM_RESERVED_MEM=24Gi",
	)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))

	patch, err := os.ReadFile(filepath.Join(tempDir, "kubeletconfiguration0+merge.yaml"))
	require.NoError(t, err)
	require.Equal(t, `apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
kubeReserved:
  cpu: "1000m"
  memory: "4Gi"
  ephemeral-storage: "1Gi"
systemReserved:
  memory: "24Gi"
`, string(patch))
}
