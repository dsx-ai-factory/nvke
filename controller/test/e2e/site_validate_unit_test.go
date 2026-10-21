// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

func TestUnit_SiteValidateFromEnvDefaults(t *testing.T) {
	settings, err := siteValidateFromEnv(envMap(nil), filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err)
	require.False(t, settings.enabled)
	require.Empty(t, settings.kubernetesVersion)
	require.Equal(t, siteNamespaces, settings.checks.namespaces)
	require.False(t, settings.checks.failFast, "the image reports every check, not just the first failure")
	require.Equal(t, "default", settings.checks.probeNamespace)
	require.Equal(t, kube.KubectlImage, settings.checks.probeImage)
	require.Equal(t, "default", settings.checks.excusedNamespace, "the runner's own namespace")
}

func TestUnit_SiteValidateFromEnvOverrides(t *testing.T) {
	settings, err := siteValidateFromEnv(envMap(map[string]string{
		siteValidateEnv:          "true",
		siteKubernetesVersionEnv: " 1.34.2 ",
		siteNamespacesEnv:        "kube-system, nke ,,cert-manager",
		siteProbeNamespaceEnv:    "probes",
		siteProbeImageEnv:        "registry.example/kubectl:1",
	}), filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err)
	require.True(t, settings.enabled)
	require.Equal(t, "1.34.2", settings.kubernetesVersion)
	require.Equal(t, []siteNamespace{
		{name: "kube-system", source: siteNamespacesEnv},
		{name: "nke", source: siteNamespacesEnv},
		{name: "cert-manager", source: siteNamespacesEnv},
	}, settings.checks.namespaces)
	require.Equal(t, "probes", settings.checks.probeNamespace)
	require.Equal(t, "registry.example/kubectl:1", settings.checks.probeImage)
}

func TestUnit_SiteValidateProbeDefaultsToOwnNamespace(t *testing.T) {
	file := filepath.Join(t.TempDir(), "namespace")
	require.NoError(t, os.WriteFile(file, []byte("validation-runner\n"), 0o600))

	settings, err := siteValidateFromEnv(envMap(nil), file)
	require.NoError(t, err)
	require.Equal(t, "validation-runner", settings.checks.probeNamespace)
	require.Equal(t, "validation-runner", settings.checks.excusedNamespace)
}

func TestUnit_SiteValidateFromEnvRejects(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"not a boolean":   {siteValidateEnv: "yes please"},
		"empty namespace": {siteNamespacesEnv: " , "},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := siteValidateFromEnv(envMap(env), filepath.Join(t.TempDir(), "absent"))
			require.Error(t, err)
		})
	}
}

func TestUnit_SiteVersionProblemsWithoutExpectedVersion(t *testing.T) {
	require.Equal(t, []string{"no expected Kubernetes version is set (" + siteKubernetesVersionEnv + ")"},
		versionProblems("", "v1.34.2", nil))
}

func envMap(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}
