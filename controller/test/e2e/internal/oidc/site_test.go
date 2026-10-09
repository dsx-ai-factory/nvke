// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

const siteTestConfig = `metricsListenAddress: 127.0.0.1:9090
oidc:
  managementOIDCAuth:
    enabled: true
    issuerURL: https://shared.example.test
  providers:
    - name: old
      issuerURL: https://old.example.test
unrelated:
  exact: keep
`

func TestSiteRejectsSnapshotUntilOriginalIsRestored(t *testing.T) {
	t.Parallel()
	deployment, configMap := siteTestObjects()
	client := fake.NewClientset(deployment, configMap)
	site, err := NewSite(client, SiteConfig{
		Namespace: "controller", Deployment: "controller", Container: "controller",
		ReadyTimeout: time.Second, PollInterval: time.Millisecond,
	})
	require.NoError(t, err)
	snapshot, err := site.Snapshot(t.Context())
	require.NoError(t, err)
	require.NoError(t, site.Apply(t.Context(), nil, false))

	client.ClearActions()
	_, err = site.Snapshot(t.Context())
	require.ErrorContains(t, err, "restore the active OIDC site edit")
	require.Empty(t, client.Actions(), "a rejected snapshot must not read or change the site")

	require.NoError(t, site.Restore(t.Context(), snapshot))
	restoredConfig, err := client.CoreV1().ConfigMaps("controller").Get(t.Context(), chartConfigMap, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, configMap.Data, restoredConfig.Data)
	restoredDeployment, err := client.AppsV1().Deployments("controller").Get(t.Context(), "controller", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, deployment.Spec.Template, restoredDeployment.Spec.Template)
	newSnapshot, err := site.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, snapshot, newSnapshot)
}

func TestSiteAppliesAndRestoresExactOwnedFields(t *testing.T) {
	t.Parallel()
	deployment, configMap := siteTestObjects()
	client := fake.NewClientset(deployment, configMap)
	resets := 0
	site, err := NewSite(client, SiteConfig{
		Namespace: "controller", Deployment: "controller", Container: "controller",
		ReadyTimeout: time.Second, PollInterval: time.Millisecond,
		ResetForwarders: func() { resets++ },
		ReadPodLogs: func(context.Context, string, string, string) ([]byte, error) {
			return []byte(`{"msg":"Loaded nke-controller-config","oidcProviders":0}` + "\n"), nil
		},
	})
	if err != nil {
		t.Fatalf("NewSite() error = %v", err)
	}
	snapshot, err := site.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	provider := Provider{Name: "test", IssuerURL: "https://issuer.test", Audiences: []string{"nke"}, UsernameClaim: "sub", CABundle: "ca"}
	if err = site.Apply(context.Background(), []Provider{provider}, false); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	updatedConfig, err := client.CoreV1().ConfigMaps("controller").Get(context.Background(), "nke-controller-config", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated ConfigMap: %v", err)
	}
	var decoded map[string]any
	if err = yaml.Unmarshal([]byte(updatedConfig.Data["config.yaml"]), &decoded); err != nil {
		t.Fatalf("decode updated config: %v", err)
	}
	if decoded["unrelated"].(map[string]any)["exact"] != "keep" {
		t.Fatalf("unrelated config = %#v", decoded["unrelated"])
	}
	oidcConfig := decoded["oidc"].(map[string]any)
	if oidcConfig["managementOIDCAuth"].(map[string]any)["enabled"] != false {
		t.Fatal("management OIDC remained enabled")
	}
	if len(oidcConfig["providers"].([]any)) != 1 {
		t.Fatalf("providers = %#v", oidcConfig["providers"])
	}

	updatedDeployment, err := client.AppsV1().Deployments("controller").Get(context.Background(), "controller", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated Deployment: %v", err)
	}
	state, err := readDeploymentState(updatedDeployment, "controller")
	if err != nil {
		t.Fatalf("readDeploymentState() error = %v", err)
	}
	require.Equal(t, deployment.Spec.Template.Spec.Containers, updatedDeployment.Spec.Template.Spec.Containers)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "controller", Name: "current", Labels: map[string]string{"app": "controller"},
			Annotations: map[string]string{defaultRestart: state.annotationValue},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "controller", Ready: true}}},
	}
	if _, err = client.CoreV1().Pods("controller").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create current pod: %v", err)
	}
	if err = site.WaitForStartupProviderCount(context.Background(), 0); err != nil {
		t.Fatalf("WaitForStartupProviderCount() error = %v", err)
	}
	if err = site.Restore(context.Background(), snapshot); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}

	restoredConfig, err := client.CoreV1().ConfigMaps("controller").Get(context.Background(), "nke-controller-config", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get restored ConfigMap: %v", err)
	}
	if restoredConfig.Data["config.yaml"] != siteTestConfig {
		t.Fatal("Restore() did not restore exact config bytes")
	}
	restoredDeployment, err := client.AppsV1().Deployments("controller").Get(context.Background(), "controller", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get restored Deployment: %v", err)
	}
	restored, err := readDeploymentState(restoredDeployment, "controller")
	if err != nil {
		t.Fatalf("read restored Deployment: %v", err)
	}
	original, err := readDeploymentState(deployment, "controller")
	if err != nil {
		t.Fatalf("read original Deployment: %v", err)
	}
	if !equalDeployment(restored, original) {
		t.Fatalf("restored state = %#v, want %#v", restored, original)
	}
	require.Equal(t, deployment.Spec.Template, restoredDeployment.Spec.Template)
	if resets != 2 {
		t.Fatalf("forwarder resets = %d, want 2", resets)
	}
}

func TestSiteTogglesConfigMapWithoutChangingLegacyEnv(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		env  []corev1.EnvVar
	}{
		{name: "absent"},
		{name: "literal true", env: []corev1.EnvVar{{Name: "NKE_OIDC_AUTH_ENABLED", Value: "true"}}},
		{name: "literal false", env: []corev1.EnvVar{{Name: "NKE_OIDC_AUTH_ENABLED", Value: "false"}}},
		{name: "value from", env: []corev1.EnvVar{{Name: "NKE_OIDC_AUTH_ENABLED", ValueFrom: &corev1.EnvVarSource{
			ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "legacy"}, Key: "enabled"},
		}}}},
		{name: "duplicate entries", env: []corev1.EnvVar{{Name: "NKE_OIDC_AUTH_ENABLED", Value: "true"}, {Name: "NKE_OIDC_AUTH_ENABLED", Value: "false"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			deployment, configMap := siteTestObjects()
			container := &deployment.Spec.Template.Spec.Containers[0]
			container.Env = append(append([]corev1.EnvVar{container.Env[0]}, test.env...), container.Env[2])
			delete(deployment.Spec.Template.Annotations, defaultRestart)
			deployment.Spec.Template.Annotations["unrelated"] = "same"
			client := fake.NewClientset(deployment, configMap)
			site, err := NewSite(client, SiteConfig{
				Namespace: "controller", Deployment: "controller", Container: "controller",
				ReadyTimeout: time.Second, PollInterval: time.Millisecond,
			})
			require.NoError(t, err)
			snapshot, err := site.Snapshot(t.Context())
			require.NoError(t, err)
			previousRestart := ""
			for _, enabled := range []bool{false, true, false} {
				require.NoError(t, site.Apply(t.Context(), nil, enabled))
				updated, getErr := client.AppsV1().Deployments("controller").Get(t.Context(), "controller", metav1.GetOptions{})
				require.NoError(t, getErr)
				require.Equal(t, container.Env, updated.Spec.Template.Spec.Containers[0].Env)
				require.NotEmpty(t, updated.Spec.Template.Annotations[defaultRestart])
				require.NotEqual(t, previousRestart, updated.Spec.Template.Annotations[defaultRestart])
				previousRestart = updated.Spec.Template.Annotations[defaultRestart]
				require.Equal(t, "same", updated.Spec.Template.Annotations["unrelated"])
				updatedConfig, getErr := client.CoreV1().ConfigMaps("controller").Get(t.Context(), chartConfigMap, metav1.GetOptions{})
				require.NoError(t, getErr)
				var decoded map[string]any
				require.NoError(t, yaml.Unmarshal([]byte(updatedConfig.Data[chartConfigKey]), &decoded))
				management := decoded["oidc"].(map[string]any)["managementOIDCAuth"].(map[string]any)
				require.Equal(t, enabled, management["enabled"])
				require.Equal(t, "https://shared.example.test", management["issuerURL"])
			}
			require.NoError(t, site.Restore(t.Context(), snapshot))
			restored, err := client.AppsV1().Deployments("controller").Get(t.Context(), "controller", metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, deployment.Spec.Template, restored.Spec.Template)
			restoredConfig, err := client.CoreV1().ConfigMaps("controller").Get(t.Context(), chartConfigMap, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, configMap.Data, restoredConfig.Data)
		})
	}
}

func TestSiteRefusesOwnedMutationAfterDeploymentUpdate(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"restart annotation", "ConfigMap key"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			deployment, configMap := siteTestObjects()
			client := fake.NewClientset(deployment, configMap)
			site, err := NewSite(client, SiteConfig{
				Namespace: "controller", Deployment: "controller", Container: "controller",
				ReadyTimeout: time.Second, PollInterval: time.Millisecond,
			})
			require.NoError(t, err)
			snapshot, err := site.Snapshot(t.Context())
			require.NoError(t, err)
			client.PrependReactor("update", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
				updated := action.(clienttesting.UpdateAction).GetObject().(*appsv1.Deployment).DeepCopy()
				mutated := updated.DeepCopy()
				if field == "restart annotation" {
					mutated.Spec.Template.Annotations[defaultRestart] = "external"
				} else {
					configMap.Data[chartConfigKey] = "external config"
					require.NoError(t, client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), configMap, "controller"))
				}
				updateErr := client.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("deployments"), mutated, "controller")
				return true, updated, updateErr
			})

			require.ErrorContains(t, site.Apply(t.Context(), nil, false), field+" changed during rollout")
			require.ErrorContains(t, site.Restore(t.Context(), snapshot), field+" changed outside the OIDC test")
			if field == "restart annotation" {
				mutated, err := client.AppsV1().Deployments("controller").Get(t.Context(), "controller", metav1.GetOptions{})
				require.NoError(t, err)
				require.Equal(t, "external", mutated.Spec.Template.Annotations[defaultRestart])
			} else {
				mutated, err := client.CoreV1().ConfigMaps("controller").Get(t.Context(), chartConfigMap, metav1.GetOptions{})
				require.NoError(t, err)
				require.Equal(t, "external config", mutated.Data[chartConfigKey])
			}
		})
	}
}

func TestSitePreservesConcurrentEnvChanges(t *testing.T) {
	t.Parallel()
	deployment, configMap := siteTestObjects()
	client := fake.NewClientset(deployment, configMap)
	site, err := NewSite(client, SiteConfig{
		Namespace: "controller", Deployment: "controller", Container: "controller",
		ReadyTimeout: time.Second, PollInterval: time.Millisecond,
	})
	require.NoError(t, err)
	snapshot, err := site.Snapshot(t.Context())
	require.NoError(t, err)
	client.PrependReactor("update", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updated := action.(clienttesting.UpdateAction).GetObject().(*appsv1.Deployment).DeepCopy()
		mutated := updated.DeepCopy()
		mutated.Spec.Template.Spec.Containers[0].Env[0].Value = "external unrelated"
		mutated.Spec.Template.Spec.Containers[0].Env[1].Value = "external"
		mutated.Spec.Template.Spec.Containers[0].Env[1].ValueFrom = nil
		mutated.Spec.Template.Annotations["unrelated"] = "external"
		updateErr := client.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("deployments"), mutated, "controller")
		return true, updated, updateErr
	})

	require.NoError(t, site.Apply(t.Context(), nil, false))
	client.ReactionChain = client.ReactionChain[1:]
	require.NoError(t, site.Restore(t.Context(), snapshot))
	mutated, err := client.AppsV1().Deployments("controller").Get(t.Context(), "controller", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "external unrelated", mutated.Spec.Template.Spec.Containers[0].Env[0].Value)
	require.Equal(t, "external", mutated.Spec.Template.Spec.Containers[0].Env[1].Value)
	require.Nil(t, mutated.Spec.Template.Spec.Containers[0].Env[1].ValueFrom)
	require.Equal(t, "external", mutated.Spec.Template.Annotations["unrelated"])
	require.Equal(t, "before", mutated.Spec.Template.Annotations[defaultRestart])
	restoredConfig, err := client.CoreV1().ConfigMaps("controller").Get(t.Context(), chartConfigMap, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, configMap.Data, restoredConfig.Data)
}

func TestSiteValidatesConfigurationBeforeWriting(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		issuerLine    string
		shared        bool
		providerCount int
		wantError     string
	}{
		{name: "missing shared issuer", shared: true, wantError: "managementOIDCAuth.issuerURL is required"},
		{name: "whitespace shared issuer", issuerLine: "    issuerURL: '   '\n", shared: true, wantError: "managementOIDCAuth.issuerURL is required"},
		{name: "disabled shared issuer can be absent"},
		{name: "provider cap exceeded", issuerLine: "    issuerURL: https://shared.example.test\n", providerCount: 6, wantError: "more than the 5-entry cap"},
		{name: "provider cap boundary", issuerLine: "    issuerURL: https://shared.example.test\n", shared: true, providerCount: 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			deployment, configMap := siteTestObjects()
			configMap.Data[chartConfigKey] = strings.Replace(siteTestConfig, "    issuerURL: https://shared.example.test\n", test.issuerLine, 1)
			client := fake.NewClientset(deployment, configMap)
			site, err := NewSite(client, SiteConfig{
				Namespace: "controller", Deployment: "controller", Container: "controller",
				ReadyTimeout: time.Second, PollInterval: time.Millisecond,
			})
			require.NoError(t, err)
			_, err = site.Snapshot(t.Context())
			require.NoError(t, err)
			providers := make([]Provider, test.providerCount)
			for i := range providers {
				providers[i] = Provider{Name: fmt.Sprintf("test-%d", i), IssuerURL: fmt.Sprintf("https://issuer-%d.test", i), Audiences: []string{"nke"}, UsernameClaim: "sub", CABundle: "ca"}
			}
			err = site.Apply(t.Context(), providers, test.shared)
			if test.wantError == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.wantError)
			currentConfig, err := client.CoreV1().ConfigMaps("controller").Get(t.Context(), chartConfigMap, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, configMap, currentConfig)
			currentDeploy, err := client.AppsV1().Deployments("controller").Get(t.Context(), "controller", metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, deployment, currentDeploy)
		})
	}
}

func TestSiteRefusesConcurrentOwnedMutation(t *testing.T) {
	t.Parallel()
	deployment, configMap := siteTestObjects()
	client := fake.NewClientset(deployment, configMap)
	site, err := NewSite(client, SiteConfig{
		Namespace: "controller", Deployment: "controller", Container: "controller",
		ReadyTimeout: time.Second, PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewSite() error = %v", err)
	}
	if _, err = site.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	mutated, err := client.CoreV1().ConfigMaps("controller").Get(context.Background(), "nke-controller-config", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	mutated.Data["config.yaml"] = "secret: do-not-print\n"
	if _, err = client.CoreV1().ConfigMaps("controller").Update(context.Background(), mutated, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update ConfigMap: %v", err)
	}
	err = site.Apply(context.Background(), nil, false)
	if err == nil || !strings.Contains(err.Error(), "changed outside") || strings.Contains(err.Error(), "do-not-print") {
		t.Fatalf("Apply() error = %v", err)
	}
}

func TestSiteRejectsMalformedConfigWithoutLeakingValues(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		config string
	}{
		{name: "invalid integer", config: "oidc: !!int private-secret-marker\n"},
		{name: "invalid mapping key", config: "? [private-secret-marker]\n: true\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			deployment, configMap := siteTestObjects()
			configMap.Data[chartConfigKey] = test.config
			client := fake.NewClientset(deployment, configMap)
			site, err := NewSite(client, SiteConfig{
				Namespace: "controller", Deployment: "controller", Container: "controller",
				ReadyTimeout: time.Second, PollInterval: time.Millisecond,
			})
			require.NoError(t, err)
			_, err = site.Snapshot(t.Context())
			require.NoError(t, err)
			client.ClearActions()

			err = site.Apply(t.Context(), nil, false)
			require.ErrorContains(t, err, "decode controller config")
			require.NotContains(t, err.Error(), "private-secret-marker")
			require.NoError(t, errors.Unwrap(err), "parser errors can contain private config values")
			for _, action := range client.Actions() {
				require.Equal(t, "get", action.GetVerb(), "invalid config must not cause API writes")
			}
			currentConfig, err := client.CoreV1().ConfigMaps("controller").Get(t.Context(), chartConfigMap, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, configMap.Data, currentConfig.Data)
			currentDeployment, err := client.AppsV1().Deployments("controller").Get(t.Context(), "controller", metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, deployment.Spec.Template, currentDeployment.Spec.Template)
		})
	}
}

func TestStartupProviderCountIgnoresStaleReadyPod(t *testing.T) {
	t.Parallel()
	deployment, configMap := siteTestObjects()
	client := fake.NewClientset(deployment, configMap)
	site, err := NewSite(client, SiteConfig{
		Namespace: "controller", Deployment: "controller", Container: "controller",
		ReadyTimeout: 20 * time.Millisecond, PollInterval: time.Millisecond,
		ReadPodLogs: func(_ context.Context, _, pod, _ string) ([]byte, error) {
			count := 0
			if pod == "current" {
				count = 1
			}
			return []byte(fmt.Sprintf(`{"msg":"Loaded nke-controller-config","oidcProviders":%d}`+"\n", count)), nil
		},
	})
	if err != nil {
		t.Fatalf("NewSite() error = %v", err)
	}
	if _, err = site.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if err = site.Apply(context.Background(), nil, false); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	updated, err := client.AppsV1().Deployments("controller").Get(context.Background(), "controller", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Deployment: %v", err)
	}
	currentRestart := updated.Spec.Template.Annotations[defaultRestart]
	for name, restart := range map[string]string{"stale": "before", "current": currentRestart} {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "controller", Name: name, Labels: map[string]string{"app": "controller"},
				Annotations: map[string]string{defaultRestart: restart},
			},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "controller", Ready: true}}},
		}
		if _, err = client.CoreV1().Pods("controller").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create %s pod: %v", name, err)
		}
	}
	err = site.WaitForStartupProviderCount(context.Background(), 0)
	if err == nil || !strings.Contains(err.Error(), "current loaded 1 OIDC providers, want 0") {
		t.Fatalf("WaitForStartupProviderCount() error = %v", err)
	}
}

func TestStartupProviderCountTimeoutReportsContextWithoutLogs(t *testing.T) {
	t.Parallel()
	deployment, configMap := siteTestObjects()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "controller", Name: "current", Labels: map[string]string{"app": "controller"},
			Annotations: map[string]string{defaultRestart: "before"},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "controller", Ready: true}}},
	}
	client := fake.NewClientset(deployment, configMap, pod)
	reads := 0
	site, err := NewSite(client, SiteConfig{
		Namespace: "controller", Deployment: "controller", Container: "controller",
		ReadyTimeout: 20 * time.Millisecond, PollInterval: time.Millisecond,
		ReadPodLogs: func(context.Context, string, string, string) ([]byte, error) {
			reads++
			return []byte("private startup output without the provider-count event"), nil
		},
	})
	require.NoError(t, err)
	_, err = site.Snapshot(t.Context())
	require.NoError(t, err)

	err = site.WaitForStartupProviderCount(t.Context(), 3)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Positive(t, reads)
	require.ErrorContains(t, err, "controller controller/controller")
	require.ErrorContains(t, err, "startup OIDC provider count 3")
	require.ErrorContains(t, err, "20ms")
	require.NotContains(t, err.Error(), "private startup output")
}

func TestStartupProviderCountReadsBoundedPodLogs(t *testing.T) {
	t.Parallel()
	startup := `{"msg":"Loaded nke-controller-config","oidcProviders":3}` + "\n"
	privateLog := "private startup output\n"
	oversized := strings.Repeat(privateLog, maxLogBytes/len(privateLog)+1)
	for _, test := range []struct {
		name      string
		logs      string
		readError bool
		witnessed bool
	}{
		{name: "short logs", logs: startup, witnessed: true},
		{name: "startup in oversized logs", logs: startup + oversized, witnessed: true},
		{name: "startup after bounded prefix", logs: oversized[:maxLogBytes] + "\n" + startup},
		{name: "startup split at boundary", logs: strings.Repeat("x", maxLogBytes-len(startup)/2-1) + "\n" + startup},
		{name: "stream read failure", logs: privateLog, readError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			deployment, configMap := siteTestObjects()
			pods := &corev1.PodList{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
				Items: []corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "controller", Name: "current", Labels: map[string]string{"app": "controller"},
						Annotations: map[string]string{defaultRestart: "before"},
					},
					Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "controller", Ready: true}}},
				}},
			}
			responses := map[string][]byte{}
			for path, object := range map[string]any{
				"/api/v1/namespaces/controller/configmaps/nke-controller-config": configMap,
				"/apis/apps/v1/namespaces/controller/deployments/controller":     deployment,
				"/api/v1/namespaces/controller/pods":                             pods,
			} {
				body, err := json.Marshal(object)
				require.NoError(t, err)
				responses[path] = body
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/namespaces/controller/pods/current/log" {
					if r.URL.Query().Get("container") != "controller" {
						http.Error(w, "incorrect container", http.StatusBadRequest)
						return
					}
					w.Header().Set("Content-Type", "text/plain")
					if test.readError {
						w.Header().Set("Content-Length", strconv.Itoa(len(test.logs)+1))
					}
					_, _ = io.WriteString(w, test.logs)
					return
				}
				body, ok := responses[r.URL.Path]
				if !ok {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
			}))
			t.Cleanup(server.Close)
			// Frequent local polling should not spend the deadline waiting for client throttling.
			clientConfig := &rest.Config{Host: server.URL, QPS: 1000, Burst: 1000}
			client, err := kubernetes.NewForConfig(clientConfig)
			require.NoError(t, err)
			site, err := NewSite(client, SiteConfig{
				Namespace: "controller", Deployment: "controller", Container: "controller",
				ReadyTimeout: 2 * time.Second, PollInterval: 20 * time.Millisecond,
			})
			require.NoError(t, err)
			_, err = site.Snapshot(t.Context())
			require.NoError(t, err)

			logs, err := site.podLogs(t.Context(), "current")
			if test.readError {
				require.ErrorIs(t, err, io.ErrUnexpectedEOF)
				require.Nil(t, logs)
			} else {
				require.NoError(t, err)
				require.Equal(t, test.logs[:min(len(test.logs), maxLogBytes)], string(logs))
			}
			err = site.WaitForStartupProviderCount(t.Context(), 3)
			if test.witnessed {
				require.NoError(t, err)
				return
			}
			if test.readError {
				require.ErrorIs(t, err, io.ErrUnexpectedEOF)
				require.ErrorContains(t, err, "read controller pod current logs")
			} else {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			}
			require.ErrorContains(t, err, "startup OIDC provider count 3")
			require.NotContains(t, err.Error(), "private startup output")
		})
	}
}

func siteTestObjects() (*appsv1.Deployment, *corev1.ConfigMap) {
	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "controller", Name: "controller", Generation: 4},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "controller"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "controller"}, Annotations: map[string]string{defaultRestart: "before"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{
					{
						Name: "controller",
						Env: []corev1.EnvVar{
							{Name: "UNCHANGED", Value: "first"},
							{Name: "NKE_OIDC_AUTH_ENABLED", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: "legacy"}, Key: "enabled",
							}}},
							{Name: "UNCHANGED_TOO", Value: "last"},
						},
					},
				}},
			},
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 4, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1},
	}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "controller", Name: "nke-controller-config"},
		Data:       map[string]string{"config.yaml": siteTestConfig, "unrelated": "same"},
	}
	return deployment, configMap
}
