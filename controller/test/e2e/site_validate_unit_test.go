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

func TestUnit_SiteValidateFromProfileDefaults(t *testing.T) {
	settings, err := siteValidateFromProfile(profileManagementCluster{}, envMap(nil), filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err)
	require.False(t, settings.enabled)
	require.Empty(t, settings.kubernetesVersion.value)
	require.Equal(t, siteNamespaces(controller.ServiceNamespace), settings.checks.namespaces)
	require.Contains(t, settings.checks.namespaces, siteNamespace{name: controller.ServiceNamespace, source: "deploy path item controller"})
	require.False(t, settings.checks.failFast, "the image reports every check, not just the first failure")
	require.Equal(t, "default", settings.checks.probeNamespace)
	require.Equal(t, probeImage{ref: kube.KubectlImage}, settings.checks.probeImage)
	require.Empty(t, settings.checks.excusedNamespace, "outside a pod there is no runner namespace to excuse, and default is checked")
	require.False(t, settings.checkIdentity)
}

func TestUnit_SiteValidateFromProfileControllerNamespace(t *testing.T) {
	settings, err := siteValidateFromProfile(profileManagementCluster{}, envMap(map[string]string{controller.EnvNamespace: "other-controller"}), filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err)
	require.Equal(t, siteNamespaces("other-controller"), settings.checks.namespaces)
	require.NotContains(t, settings.checks.namespaces, siteNamespace{name: controller.ServiceNamespace, source: "deploy path item controller"},
		"the default list takes the controller's namespace from the settings it reads, not the process environment")
}

func TestUnit_SiteValidateFromProfileOverrides(t *testing.T) {
	settings, err := siteValidateFromProfile(profileManagementCluster{
		profileExpectations: profileExpectations{Namespaces: []string{"kube-system", " nvke-controller ", "", "cert-manager"}},
		KubernetesVersion:   " 1.34.2 ",
		Probe: profileManagementProbe{
			Namespace:    "probes",
			profileProbe: profileProbe{Image: "registry.example/kubectl:1", PullSecrets: []string{" mirror", " ", "registry"}},
		},
	}, envMap(map[string]string{siteValidateEnv: "true"}), filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err)
	require.True(t, settings.enabled)
	require.Equal(t, resolvedVersion{field: "managementCluster.kubernetesVersion", value: "1.34.2", source: "profile"}, settings.kubernetesVersion)
	require.Equal(t, []siteNamespace{
		{name: "kube-system", source: "managementCluster.namespaces"},
		{name: "nvke-controller", source: "managementCluster.namespaces"},
		{name: "cert-manager", source: "managementCluster.namespaces"},
	}, settings.checks.namespaces)
	require.Equal(t, "probes", settings.checks.probeNamespace)
	require.Equal(t, probeImage{ref: "registry.example/kubectl:1", pullSecrets: []corev1.LocalObjectReference{{Name: "mirror"}, {Name: "registry"}}}, settings.checks.probeImage)
	require.Empty(t, settings.checks.excusedNamespace, "a probe namespace given is still checked")
}

func TestUnit_SiteKubernetesVersionEnvOverridesProfile(t *testing.T) {
	settings, err := siteValidateFromProfile(profileManagementCluster{KubernetesVersion: "1.34.2"},
		envMap(map[string]string{siteKubernetesVersionEnv: "1.35.0"}), filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err)
	require.Equal(t, "1.35.0", settings.kubernetesVersion.value)
	require.Contains(t, settings.describe(), "managementCluster.kubernetesVersion = 1.35.0 (NVKE_MGMT_CLUSTER_KUBERNETES_VERSION, overriding the profile's 1.34.2)")

	settings, err = siteValidateFromProfile(profileManagementCluster{}, envMap(nil), filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err)
	require.Contains(t, settings.describe(), "managementCluster.kubernetesVersion is unset")
}

func TestUnit_SiteValidateExcusesOnlyOwnNamespace(t *testing.T) {
	file := filepath.Join(t.TempDir(), "namespace")
	require.NoError(t, os.WriteFile(file, []byte("validation-runner\n"), 0o600))

	settings, err := siteValidateFromProfile(profileManagementCluster{Probe: profileManagementProbe{Namespace: "probes"}}, envMap(nil), file)
	require.NoError(t, err)
	require.Equal(t, "probes", settings.checks.probeNamespace)
	require.Equal(t, "validation-runner", settings.checks.excusedNamespace)
	require.True(t, settings.checkIdentity)
}

func TestUnit_SiteValidateProbeDefaultsToOwnNamespace(t *testing.T) {
	file := filepath.Join(t.TempDir(), "namespace")
	require.NoError(t, os.WriteFile(file, []byte("validation-runner\n"), 0o600))

	settings, err := siteValidateFromProfile(profileManagementCluster{}, envMap(nil), file)
	require.NoError(t, err)
	require.Equal(t, "validation-runner", settings.checks.probeNamespace)
	require.Equal(t, "validation-runner", settings.checks.excusedNamespace)
}

func TestUnit_SiteValidateFromProfileRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		profile profileManagementCluster
		env     map[string]string
		want    string
	}{
		"not a boolean":   {env: map[string]string{siteValidateEnv: "yes please"}, want: siteValidateEnv},
		"empty namespace": {profile: profileManagementCluster{profileExpectations: profileExpectations{Namespaces: []string{" "}}}, want: "managementCluster.namespaces names no namespace"},
		"unknown scope":   {profile: profileManagementCluster{Scope: "namespace"}, want: `managementCluster.scope: want cluster or namespaces, got "namespace"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := siteValidateFromProfile(tc.profile, envMap(tc.env), filepath.Join(t.TempDir(), "absent"))
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestUnit_SiteValidateScope(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent")
	for _, raw := range []string{"", " cluster "} {
		settings, err := siteValidateFromProfile(profileManagementCluster{Scope: raw}, envMap(nil), absent)
		require.NoError(t, err)
		require.Nil(t, settings.checks.scope, "%q checks the whole cluster", raw)
	}

	settings, err := siteValidateFromProfile(profileManagementCluster{
		Scope: "namespaces",
		profileExpectations: profileExpectations{
			Namespaces: []string{"kube-system", "nvke-controller"},
			Components: []profileComponent{{Name: componentArgoCD}, {Name: componentCNPG, Namespace: "databases"}},
		},
	}, envMap(map[string]string{controller.EnvNamespace: "nvke-controller"}), absent)
	require.NoError(t, err)
	require.Equal(t, siteScope{"kube-system": true, "nvke-controller": true, "argocd": true, "databases": true}, settings.checks.scope,
		"the required namespaces and each declared component's own")
	require.Contains(t, settings.describe(), "managementCluster.scope: namespaces: argocd, databases, kube-system, nvke-controller")

	settings, err = siteValidateFromProfile(profileManagementCluster{
		Scope:               "namespaces",
		profileExpectations: profileExpectations{Namespaces: []string{"team-a"}},
	}, envMap(nil), absent)
	require.NoError(t, err)
	require.Equal(t, siteScope{"team-a": true, "kube-system": true, controller.ServiceNamespace: true}, settings.checks.scope,
		"a list without kube-system or the controller's namespace still scopes them in")
}

func TestUnit_SiteSplitScope(t *testing.T) {
	pods := []corev1.Pod{podIn("nvke-controller"), podIn("other-team"), podIn("kube-system")}
	namespace := func(pod corev1.Pod) string { return pod.Namespace }

	in, out := splitScope(nil, pods, namespace)
	require.Equal(t, pods, in, "no scope keeps everything in")
	require.Empty(t, out)

	in, out = splitScope(siteScope{"nvke-controller": true, "kube-system": true}, pods, namespace)
	require.Equal(t, []corev1.Pod{podIn("nvke-controller"), podIn("kube-system")}, in)
	require.Equal(t, []corev1.Pod{podIn("other-team")}, out)
}

func podIn(namespace string) corev1.Pod {
	pod := corev1.Pod{}
	pod.Namespace, pod.Name = namespace, "p"
	return pod
}

func TestUnit_SiteVersionProblemsWithoutExpectedVersion(t *testing.T) {
	require.Equal(t, []string{"no expected Kubernetes version is set (managementCluster.kubernetesVersion or " + siteKubernetesVersionEnv + ")"},
		versionProblems("", "v1.34.2", nil))
}

func envMap(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}
