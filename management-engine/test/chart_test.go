//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package deployment_test

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func render(t *testing.T, command string, args ...string) []unstructured.Unstructured {
	t.Helper()
	data, err := exec.CommandContext(t.Context(), command, args...).CombinedOutput()
	require.NoError(t, err, "%s", data)
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var objects []unstructured.Unstructured
	for {
		var obj unstructured.Unstructured
		err := decoder.Decode(&obj)
		if err == io.EOF {
			return objects
		}
		require.NoError(t, err)
		if len(obj.Object) != 0 {
			objects = append(objects, obj)
		}
	}
}

func kustomize() string {
	if path := os.Getenv("KUSTOMIZE"); path != "" {
		return path
	}
	return "../bin/kustomize"
}

func TestRuntimeBoundary(t *testing.T) {
	const stateNamespace = "management-state-system"
	for _, tt := range []struct {
		name    string
		objects []unstructured.Unstructured
	}{
		{"helm", renderChart(t, "--include-crds")},
		{"kustomize", render(t, kustomize(), "build", "../config/default")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var found bool
			var namespaces int
			for _, obj := range tt.objects {
				if obj.GetKind() == "Namespace" {
					require.Equal(t, "kustomize", tt.name, "only the full Kustomize install owns a namespace")
					require.Equal(t, "management-engine-system", obj.GetName())
					namespaces++
					continue
				}
				require.Contains(t, []string{"Deployment", "ServiceAccount", "Service", "NetworkPolicy"}, obj.GetKind(), "datastore resources must not be installed in the services cluster")
				if obj.GetKind() == "ServiceAccount" {
					require.Equal(t, false, obj.Object["automountServiceAccountToken"])
					continue
				}
				if obj.GetKind() != "Deployment" {
					continue
				}
				found = true
				var deployment appsv1.Deployment
				require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &deployment))
				pod := deployment.Spec.Template.Spec
				require.NotNil(t, pod.AutomountServiceAccountToken)
				require.False(t, *pod.AutomountServiceAccountToken)
				require.Greater(t, *pod.TerminationGracePeriodSeconds, int64(30))
				require.Len(t, pod.Containers, 1)
				container := pod.Containers[0]
				require.ElementsMatch(t, []string{
					"--leader-elect", "--leader-election-namespace=$(STATE_NAMESPACE)",
					"--kubeconfig=/etc/management-api/kubeconfig", "--health-probe-bind-address=:8081", "--metrics-bind-address=:8443",
				}, container.Args, "rendered args must match the manager CLI")
				require.NotEqual(t, stateNamespace, obj.GetNamespace(), "exercise different host/state namespaces")
				var namespace string
				for _, env := range container.Env {
					if env.Name == "STATE_NAMESPACE" {
						require.Nil(t, env.ValueFrom)
						namespace = env.Value
					}
				}
				require.Equal(t, stateNamespace, namespace)
				require.Len(t, pod.Volumes, 1)
				require.NotNil(t, pod.Volumes[0].Secret)
				require.Equal(t, "management-state-client", pod.Volumes[0].Secret.SecretName)
				require.Len(t, container.VolumeMounts, 1)
				require.Equal(t, pod.Volumes[0].Name, container.VolumeMounts[0].Name)
				require.Equal(t, "/etc/management-api", container.VolumeMounts[0].MountPath)
				require.True(t, container.VolumeMounts[0].ReadOnly)
			}
			require.True(t, found)
			if tt.name == "kustomize" {
				require.Equal(t, 1, namespaces, "fresh installs must create the runtime namespace")
			}
		})
	}
}

func TestUndeployPreservesNamespace(t *testing.T) {
	// Capture the deletion manifest without contacting a cluster.
	kubectl := filepath.Join(t.TempDir(), "kubectl")
	require.NoError(t, os.WriteFile(kubectl, []byte("#!/bin/sh\ncat\n"), 0o700))
	objects := render(t, "make", "-s", "-C", "..", "undeploy", "HOST_CONTEXT=unused", "KUBECTL="+kubectl)
	require.NotEmpty(t, objects)
	for _, obj := range objects {
		require.Contains(t, []string{"Deployment", "ServiceAccount", "Service", "NetworkPolicy"}, obj.GetKind())
		require.Equal(t, "management-engine-system", obj.GetNamespace())
	}
}

func TestHelmDeployImage(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, tt := range []struct{ image, want string }{
		{"registry.example/engine:v1", "registry.example/engine:v1"},
		{"registry.example:5000/engine:v1", "registry.example:5000/engine:v1"},
		{"registry.example/engine@" + digest, "registry.example/engine@" + digest},
		{"registry.example:5000/engine:v1@" + digest, "registry.example:5000/engine:v1@" + digest},
		{"engine", "engine:latest"},
		{"registry.example:5000/engine", "registry.example:5000/engine:latest"},
	} {
		t.Run(tt.image, func(t *testing.T) {
			// Exercise the Makefile's image handling without performing an upgrade.
			cmd := exec.CommandContext(t.Context(), "make", "-s", "-C", "..", "helm-deploy", "HOST_CONTEXT=unused", "HELM=echo", "IMG="+tt.image)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
			var args []string
			for _, arg := range strings.Fields(string(output)) {
				if strings.HasPrefix(arg, "manager.image.") {
					args = append(args, "--set-string", arg)
				}
			}
			require.Len(t, args, 4)
			var found bool
			for _, obj := range renderChart(t, args...) {
				if obj.GetKind() == "Deployment" {
					var deployment appsv1.Deployment
					require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &deployment))
					require.Equal(t, tt.want, deployment.Spec.Template.Spec.Containers[0].Image)
					found = true
				}
			}
			require.True(t, found)
		})
	}
}

func renderChart(t *testing.T, args ...string) []unstructured.Unstructured {
	t.Helper()
	base := []string{"template", "engine", "../chart", "--namespace", "services-system", "-f", "../config/samples/runtime-values.yaml"}
	return render(t, "helm", append(base, args...)...)
}

func TestServiceAccountSelection(t *testing.T) {
	for _, tt := range []struct {
		name, values, want string
		create             bool
	}{
		{"created", "serviceAccount.enabled=true,serviceAccount.name=existing-account", "engine-management-engine-controller-manager", true},
		{"existing", "serviceAccount.enabled=false,serviceAccount.name=existing-account", "existing-account", false},
		{"unnamed", "serviceAccount.enabled=false,serviceAccount.name=", "engine-management-engine-controller-manager", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var accounts []string
			var selected string
			for _, obj := range renderChart(t, "--set", tt.values) {
				switch obj.GetKind() {
				case "ServiceAccount":
					accounts = append(accounts, obj.GetName())
				case "Deployment":
					var err error
					selected, _, err = unstructured.NestedString(obj.Object, "spec", "template", "spec", "serviceAccountName")
					require.NoError(t, err)
				}
			}
			require.Equal(t, tt.want, selected)
			if tt.create {
				require.Equal(t, []string{tt.want}, accounts)
			} else {
				require.Empty(t, accounts)
			}
		})
	}
}

func TestMetricsConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name                                string
		args                                []string
		monitor, disabled, insecure, policy bool
	}{
		{name: "defaults"},
		{name: "prometheus", args: []string{"--set", "prometheus.enabled=true"}, monitor: true},
		{name: "disabled", args: []string{"--set", "metrics.enabled=false"}, disabled: true},
		{name: "http opt-in", args: []string{"--set", "metrics.secure=false"}, insecure: true},
		{name: "network policy", args: []string{"--set", "networkPolicy.enabled=true"}, policy: true},
		{name: "custom name", args: []string{"--set", "nameOverride=custom-engine,fullnameOverride=custom-release"}},
		{name: "kustomize", monitor: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var objects []unstructured.Unstructured
			if tt.name == "kustomize" {
				objects = append(render(t, kustomize(), "build", "../config/default"), render(t, kustomize(), "build", "../config/prometheus")...)
			} else {
				objects = renderChart(t, tt.args...)
			}
			byKind := map[string]unstructured.Unstructured{}
			for _, obj := range objects {
				byKind[obj.GetKind()] = obj
			}
			var deployment appsv1.Deployment
			require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(byKind["Deployment"].Object, &deployment))
			args := deployment.Spec.Template.Spec.Containers[0].Args
			if tt.disabled {
				require.Contains(t, args, "--metrics-bind-address=0")
				require.NotContains(t, byKind, "Service")
			} else {
				require.Contains(t, args, "--metrics-bind-address=:8443")
				if tt.insecure {
					require.Contains(t, args, "--metrics-secure=false")
				} else {
					require.NotContains(t, args, "--metrics-secure=false")
				}
				var service corev1.Service
				require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(byKind["Service"].Object, &service))
				require.Equal(t, deployment.Namespace, service.Namespace)
				for key, value := range service.Spec.Selector {
					require.Equal(t, value, deployment.Spec.Template.Labels[key])
				}
				require.NotEmpty(t, service.Spec.Selector)
				require.Len(t, service.Spec.Ports, 1)
				require.Equal(t, int32(8443), service.Spec.Ports[0].Port)
				require.Equal(t, int32(8443), service.Spec.Ports[0].TargetPort.IntVal)
				name := "https"
				if tt.insecure {
					name = "http"
				}
				require.Equal(t, name, service.Spec.Ports[0].Name)
			}
			policyObj, hasPolicy := byKind["NetworkPolicy"]
			require.Equal(t, tt.policy, hasPolicy)
			if hasPolicy {
				var policy networkingv1.NetworkPolicy
				require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(policyObj.Object, &policy))
				require.Len(t, policy.Spec.Ingress, 1)
				rule := policy.Spec.Ingress[0]
				require.Len(t, rule.From, 1)
				require.Equal(t, map[string]string{"metrics": "enabled"}, rule.From[0].NamespaceSelector.MatchLabels)
				require.Equal(t, int32(8443), rule.Ports[0].Port.IntVal)
			}
			monitor, hasMonitor := byKind["ServiceMonitor"]
			require.Equal(t, tt.monitor, hasMonitor)
			if hasMonitor {
				endpoints, _, err := unstructured.NestedSlice(monitor.Object, "spec", "endpoints")
				require.NoError(t, err)
				require.Len(t, endpoints, 1)
				endpoint := endpoints[0].(map[string]any)
				require.Equal(t, "https", endpoint["scheme"])
				require.Equal(t, "https", endpoint["port"])
				require.Equal(t, "/var/run/secrets/kubernetes.io/serviceaccount/token", endpoint["bearerTokenFile"])
			}
		})
	}
}

func TestChartRequiresRemoteConfiguration(t *testing.T) {
	for _, missing := range []string{"manager.env[0].value=", "manager.extraVolumes[0].secret.secretName="} {
		data, err := exec.CommandContext(t.Context(), "helm", "template", "engine", "../chart", "-f", "../config/samples/runtime-values.yaml", "--set-string", missing).CombinedOutput()
		require.Error(t, err, "must reject missing remote configuration: %s", data)
		require.Contains(t, string(data), "schema")
	}
}

func TestStateBoundary(t *testing.T) {
	objects := render(t, kustomize(), "build", "../config/state")
	counts := map[string]int{}
	var foundControllerRole, foundControllerBinding bool
	for _, obj := range objects {
		counts[obj.GetKind()]++
		require.Contains(t, []string{"Namespace", "CustomResourceDefinition", "ServiceAccount", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding"}, obj.GetKind())
		if obj.GetNamespace() != "" {
			require.Equal(t, "management-state-system", obj.GetNamespace())
		}
		switch obj.GetKind() {
		case "Namespace":
			require.Equal(t, "management-state-system", obj.GetName())
		case "ClusterRole":
			if obj.GetName() == "management-engine-manager-role" {
				foundControllerRole = true
				var role rbacv1.ClusterRole
				require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &role))
				require.Equal(t, []rbacv1.PolicyRule{{
					APIGroups: []string{"internal.nke.nvidia.com"},
					Resources: []string{"nkeclusters", "nodepools"},
					Verbs:     []string{"get", "list", "watch"},
				}}, role.Rules, "bootstrap controllers only need read access")
			}
		case "RoleBinding", "ClusterRoleBinding":
			subjects, _, err := unstructured.NestedSlice(obj.Object, "subjects")
			require.NoError(t, err)
			require.Equal(t, []any{map[string]any{"kind": "ServiceAccount", "name": "management-engine-controller-manager", "namespace": "management-state-system"}}, subjects)
			if obj.GetName() == "management-engine-manager-rolebinding" {
				foundControllerBinding = true
				require.Equal(t, "ClusterRoleBinding", obj.GetKind())
				require.Equal(t, map[string]any{
					"apiGroup": rbacv1.GroupName, "kind": "ClusterRole", "name": "management-engine-manager-role",
				}, obj.Object["roleRef"])
			}
		}
	}
	require.True(t, foundControllerRole, "datastore must include controller read permissions")
	require.True(t, foundControllerBinding, "datastore must bind controller permissions")
	for _, kind := range []string{"CustomResourceDefinition", "ServiceAccount", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding", "Namespace"} {
		require.Positive(t, counts[kind], "datastore bundle must include %s", kind)
	}
	crds, err := filepath.Glob("../config/crd/bases/*.yaml")
	require.NoError(t, err)
	require.Equal(t, len(crds), counts["CustomResourceDefinition"], "install every generated CRD")
}
