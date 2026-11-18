// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

func TestUnit_SiteValidateFromEnvDefaults(t *testing.T) {
	settings, err := siteValidateFromEnv(envMap(nil), filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err)
	require.False(t, settings.enabled)
	require.Empty(t, settings.kubernetesVersion)
	require.Equal(t, siteNamespaces(controller.ServiceNamespace), settings.checks.namespaces)
	require.Contains(t, settings.checks.namespaces, siteNamespace{name: controller.ServiceNamespace, source: "deploy path item controller"})
	require.False(t, settings.checks.failFast, "the image reports every check, not just the first failure")
	require.Equal(t, "default", settings.checks.probeNamespace)
	require.Equal(t, probeImage{ref: kube.KubectlImage}, settings.checks.probeImage)
	require.Empty(t, settings.checks.excusedNamespace, "outside a pod there is no runner namespace to excuse, and default is checked")
	require.False(t, settings.checkIdentity)
}

func TestUnit_SiteValidateFromEnvControllerNamespace(t *testing.T) {
	settings, err := siteValidateFromEnv(envMap(map[string]string{controller.EnvNamespace: "my-controller"}), filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err)
	require.Equal(t, siteNamespaces("my-controller"), settings.checks.namespaces)
	require.NotContains(t, settings.checks.namespaces, siteNamespace{name: controller.ServiceNamespace, source: "deploy path item controller"},
		"the default list takes the controller's namespace from the settings it reads, not the process environment")
}

func TestUnit_SiteValidateFromEnvOverrides(t *testing.T) {
	settings, err := siteValidateFromEnv(envMap(map[string]string{
		siteValidateEnv:          "true",
		siteKubernetesVersionEnv: " 1.34.2 ",
		siteNamespacesEnv:        "kube-system, nke ,,cert-manager",
		siteProbeNamespaceEnv:    "probes",
		siteProbeImageEnv:        "registry.example/kubectl:1",
		siteProbePullSecretsEnv:  " mirror, ,ngc",
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
	require.Equal(t, probeImage{ref: "registry.example/kubectl:1", pullSecrets: []corev1.LocalObjectReference{{Name: "mirror"}, {Name: "ngc"}}}, settings.checks.probeImage)
	require.Empty(t, settings.checks.excusedNamespace, "a probe namespace given is still checked")
}

func TestUnit_SiteValidateExcusesOnlyOwnNamespace(t *testing.T) {
	file := filepath.Join(t.TempDir(), "namespace")
	require.NoError(t, os.WriteFile(file, []byte("validation-runner\n"), 0o600))

	settings, err := siteValidateFromEnv(envMap(map[string]string{siteProbeNamespaceEnv: "probes"}), file)
	require.NoError(t, err)
	require.Equal(t, "probes", settings.checks.probeNamespace)
	require.Equal(t, "validation-runner", settings.checks.excusedNamespace)
	require.True(t, settings.checkIdentity)
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
		"unknown scope":   {siteScopeEnv: "nke"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := siteValidateFromEnv(envMap(env), filepath.Join(t.TempDir(), "absent"))
			require.Error(t, err)
		})
	}
}

func TestUnit_SiteValidateScope(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent")
	for _, raw := range []string{"", " cluster "} {
		settings, err := siteValidateFromEnv(envMap(map[string]string{siteScopeEnv: raw}), absent)
		require.NoError(t, err)
		require.Nil(t, settings.checks.scope, "%q checks the whole cluster", raw)
	}

	settings, err := siteValidateFromEnv(envMap(map[string]string{
		siteScopeEnv:            "namespaces",
		siteNamespacesEnv:       "kube-system,nke",
		siteComponentsEnv:       "argocd,cnpg=databases",
		controller.EnvNamespace: "nke",
	}), absent)
	require.NoError(t, err)
	require.Equal(t, siteScope{"kube-system": true, "nke": true, "argocd": true, "databases": true}, settings.checks.scope,
		"the required namespaces and each declared component's own")
	require.Contains(t, settings.describe(), siteScopeEnv+"=namespaces: argocd, databases, kube-system, nke")

	settings, err = siteValidateFromEnv(envMap(map[string]string{
		siteScopeEnv:      "namespaces",
		siteNamespacesEnv: "team-a",
	}), absent)
	require.NoError(t, err)
	require.Equal(t, siteScope{"team-a": true, "kube-system": true, controller.ServiceNamespace: true}, settings.checks.scope,
		"a list without kube-system or the controller's namespace still scopes them in")
}

func TestUnit_SiteSplitScope(t *testing.T) {
	pods := []corev1.Pod{podIn("nke"), podIn("other-team"), podIn("kube-system")}
	namespace := func(pod corev1.Pod) string { return pod.Namespace }

	in, out := splitScope(nil, pods, namespace)
	require.Equal(t, pods, in, "no scope keeps everything in")
	require.Empty(t, out)

	in, out = splitScope(siteScope{"nke": true, "kube-system": true}, pods, namespace)
	require.Equal(t, []corev1.Pod{podIn("nke"), podIn("kube-system")}, in)
	require.Equal(t, []corev1.Pod{podIn("other-team")}, out)
}

func podIn(namespace string) corev1.Pod {
	pod := corev1.Pod{}
	pod.Namespace, pod.Name = namespace, "p"
	return pod
}

func TestUnit_SiteVersionProblemsWithoutExpectedVersion(t *testing.T) {
	require.Equal(t, []string{"no expected Kubernetes version is set (" + siteKubernetesVersionEnv + ")"},
		versionProblems("", "v1.34.2", nil))
}

func envMap(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}
