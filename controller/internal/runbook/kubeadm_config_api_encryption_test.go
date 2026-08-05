// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package runbook

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const kubeadmConfigEncryptionRunbook = "kubeadm-config-api-encryption"

func TestKubeadmConfigAPIEncryptionPreservesVersionSpecificExtraArgsShape(t *testing.T) {
	if err := exec.CommandContext(context.Background(), "python3", "-c", "import yaml").Run(); err != nil {
		t.Skip("PyYAML is not installed in the local test environment")
	}

	tests := []struct {
		name         string
		apiVersion   string
		validateOnly bool
		wantList     bool
	}{
		{name: "v1beta3 map", apiVersion: "kubeadm.k8s.io/v1beta3"},
		{name: "v1beta4 list", apiVersion: "kubeadm.k8s.io/v1beta4", validateOnly: true, wantList: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := runKubeadmConfigAPIEncryption(t, tt.apiVersion, tt.validateOnly)

			var config map[string]any
			require.NoError(t, yaml.Unmarshal(result.validatedConfig, &config))
			apiServer, ok := config["apiServer"].(map[string]any)
			require.True(t, ok)
			extraArgs := apiServer["extraArgs"]

			if tt.wantList {
				args, ok := extraArgs.([]any)
				require.True(t, ok, "v1beta4 extraArgs must be a list")
				assertExtraArgInList(t, args, "audit-log-path", "/var/log/kubernetes/audit.log")
				assertExtraArgInList(t, args, "encryption-provider-config", "/etc/kubernetes/kube-api-encryption/encryption-config.yaml")
				assertExtraArgInList(t, args, "encryption-provider-config-automatic-reload", "true")
			} else {
				args, ok := extraArgs.(map[string]any)
				require.True(t, ok, "v1beta3 extraArgs must be a map")
				assert.Equal(t, "/var/log/kubernetes/audit.log", args["audit-log-path"])
				assert.Equal(t, "/etc/kubernetes/kube-api-encryption/encryption-config.yaml", args["encryption-provider-config"])
				assert.Equal(t, "true", args["encryption-provider-config-automatic-reload"])
			}

			if tt.validateOnly {
				assert.False(t, result.applied, "validate-only mode must not update the ConfigMap")
			} else {
				assert.True(t, result.applied)
			}
		})
	}
}

type kubeadmConfigRunbookResult struct {
	validatedConfig []byte
	applied         bool
}

func runKubeadmConfigAPIEncryption(t *testing.T, apiVersion string, validateOnly bool) kubeadmConfigRunbookResult {
	t.Helper()

	tempDir := t.TempDir()
	binDir := filepath.Join(tempDir, "bin")
	require.NoError(t, os.Mkdir(binDir, 0o755))

	configMapPath := filepath.Join(tempDir, "configmap.json")
	validatedPath := filepath.Join(tempDir, "validated.yaml")
	appliedPath := filepath.Join(tempDir, "applied.json")
	scriptPath := filepath.Join(tempDir, "runbook.sh")

	clusterConfiguration := "apiVersion: " + apiVersion + `
kind: ClusterConfiguration
apiServer:
  extraArgs:
    audit-log-path: /var/log/kubernetes/audit.log
`
	configMap, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "kubeadm-config",
			"namespace": "kube-system",
		},
		"data": map[string]string{"ClusterConfiguration": clusterConfiguration},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configMapPath, configMap, 0o600))

	writeExecutable(t, filepath.Join(binDir, "kubectl"), `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  get) cat "$CONFIGMAP_JSON_FILE" ;;
  apply) cp "$3" "$APPLIED_FILE" ;;
  *) exit 64 ;;
esac
`)
	writeExecutable(t, filepath.Join(binDir, "kubeadm"), `#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == "config" && "$2" == "validate" && "$3" == "--config" ]]
cp "$4" "$KUBEADM_VALIDATED_FILE"
`)

	script, err := Script(kubeadmConfigEncryptionRunbook)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o700))

	validateOnlyValue := "false"
	if validateOnly {
		validateOnlyValue = "true"
	}
	cmd := exec.CommandContext(context.Background(), "bash", scriptPath)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"CONFIGMAP_JSON_FILE="+configMapPath,
		"KUBEADM_VALIDATED_FILE="+validatedPath,
		"APPLIED_FILE="+appliedPath,
		"KUBEADM_CONFIG_API_ENCRYPTION_VALIDATE_ONLY="+validateOnlyValue,
	)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "runbook output:\n%s", output)

	validated, err := os.ReadFile(validatedPath)
	require.NoError(t, err)
	_, appliedErr := os.Stat(appliedPath)
	return kubeadmConfigRunbookResult{
		validatedConfig: validated,
		applied:         appliedErr == nil,
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o700))
}

func assertExtraArgInList(t *testing.T, args []any, name, value string) {
	t.Helper()
	for _, raw := range args {
		arg, ok := raw.(map[string]any)
		if ok && arg["name"] == name {
			assert.Equal(t, value, arg["value"])
			return
		}
	}
	assert.Failf(t, "missing extraArg", "name=%s", name)
}
