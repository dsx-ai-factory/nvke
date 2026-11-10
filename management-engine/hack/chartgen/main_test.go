// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// TestGenerationPropagatesSources exercises source edits through plugin generation and Helm rendering.
func TestGenerationPropagatesSources(t *testing.T) {
	for _, omitEnv := range []bool{false, true} {
		name := "environment present"
		if omitEnv {
			name = "environment omitted"
		}
		t.Run(name, func(t *testing.T) { checkSourcePropagation(t, omitEnv) })
	}
}

func checkSourcePropagation(t *testing.T, omitEnv bool) {
	binary := os.Getenv("KUBEBUILDER")
	if binary == "" {
		t.Skip("make chart-check supplies the pinned Kubebuilder executable")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(t.TempDir(), "management-engine")
	for _, path := range []string{"PROJECT", "config", "hack/chart", "chart/Chart.yaml", "chart/Chart.lock"} {
		if err := copyPath(filepath.Join(root, path), filepath.Join(fixture, path)); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"LICENSE", "NOTICE", "controller/deploy/charts/nvke-crds"} {
		if err := copyPath(filepath.Join(root, "..", path), filepath.Join(fixture, "..", path)); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(root, "hack/boilerplate.yaml.txt")
	if err := copyFile(source, filepath.Join(fixture, "hack/boilerplate.yaml.txt")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(fixture)
	run := func(program string, args ...string) []byte {
		t.Helper()
		data, err := exec.Command(program, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", program, args, err, data)
		}
		return data
	}
	run("git", "init", "-q")
	changeCanonicalSources(t, omitEnv)
	if err := os.MkdirAll("dist", 0o755); err != nil {
		t.Fatal(err)
	}
	input := run(os.Getenv("KUSTOMIZE"), "build", "config/default")
	if err := os.WriteFile("dist/install.yaml", input, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := generate(binary, false); err != nil {
		t.Fatal(err)
	}
	if err := generate(binary, true); err != nil {
		t.Fatalf("generation is not deterministic: %v", err)
	}
	run(os.Getenv("HELM"), "dependency", "build", "chart", "--skip-refresh")
	output := run(os.Getenv("HELM"), "template", "source-check", "chart", "-f", "config/samples/runtime-values.yaml")
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	var deployment, permission bool
	for {
		var obj unstructured.Unstructured
		if err := decoder.Decode(&obj); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if obj.GetKind() == "Deployment" {
			assertSourceDeployment(t, obj, omitEnv)
			deployment = true
		}
		if obj.GetKind() == "ClusterRole" {
			data, err := yaml.Marshal(obj.Object["rules"])
			if err != nil {
				t.Fatal(err)
			}
			permission = permission || strings.Contains(string(data), "sourcechecks")
		}
	}
	if !deployment || !permission {
		t.Fatalf("canonical edits did not propagate: deployment=%t permission=%t", deployment, permission)
	}
	if err := os.WriteFile("chart/templates/manager/manager.yaml", []byte("manual edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := generate(binary, true); err == nil || !strings.Contains(err.Error(), "generation drift") {
		t.Fatalf("manual chart drift was not rejected: %v", err)
	}
}

func changeCanonicalSources(t *testing.T, omitEnv bool) {
	t.Helper()
	input, err := os.ReadFile("config/manager/manager.yaml")
	if err != nil {
		t.Fatal(err)
	}
	object := map[string]any{}
	if err := yaml.Unmarshal(input, &object); err != nil {
		t.Fatal(err)
	}
	containers, _, _ := unstructured.NestedSlice(object, "spec", "template", "spec", "containers")
	manager := containers[0].(map[string]any)
	if err := unstructured.SetNestedField(manager, "25m", "resources", "requests", "cpu"); err != nil {
		t.Fatal(err)
	}
	env, _, _ := unstructured.NestedSlice(manager, "env")
	manager["env"] = append(env, map[string]any{"name": "SOURCE_PROPAGATION", "value": "present"})
	if omitEnv {
		delete(manager, "env")
	}
	manager["envFrom"] = []any{map[string]any{"configMapRef": map[string]any{"name": "source-env-settings"}}}
	args, _, _ := unstructured.NestedSlice(manager, "args")
	manager["args"] = append(args, "--nvke-controller-endpoint=https://source.invalid/api",
		"--infra-template-configmap", "source-template")

	if err := unstructured.SetNestedField(manager, int64(37), "readinessProbe", "initialDelaySeconds"); err != nil {
		t.Fatal(err)
	}
	mounts, _, _ := unstructured.NestedSlice(manager, "volumeMounts")
	manager["volumeMounts"] = append(mounts, map[string]any{
		"name": "source-config", "mountPath": "/etc/source-settings",
	})
	volumes, _, _ := unstructured.NestedSlice(object, "spec", "template", "spec", "volumes")
	volumes = append(volumes, map[string]any{
		"name": "source-config", "configMap": map[string]any{"name": "source-settings"},
	})
	if err := unstructured.SetNestedSlice(object, volumes, "spec", "template", "spec", "volumes"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedSlice(object, containers, "spec", "template", "spec", "containers"); err != nil {
		t.Fatal(err)
	}
	input, err = yaml.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("config/manager/manager.yaml", input, 0o644); err != nil {
		t.Fatal(err)
	}
	role, err := os.ReadFile("config/rbac/role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	role = append(role, []byte("\n- apiGroups: [example.test]\n  resources: [sourcechecks]\n  verbs: [get]\n")...)
	if err := os.WriteFile("config/rbac/role.yaml", role, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertSourceDeployment(t *testing.T, obj unstructured.Unstructured, omitEnv bool) {
	t.Helper()
	containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	manager := containers[0].(map[string]any)
	cpu, _, _ := unstructured.NestedString(manager, "resources", "requests", "cpu")
	if cpu != "25m" {
		t.Errorf("source CPU request did not propagate: %q", cpu)
	}
	delay, _, _ := unstructured.NestedInt64(manager, "readinessProbe", "initialDelaySeconds")
	if delay != 37 {
		t.Errorf("source readiness delay did not propagate: %d", delay)
	}
	data, err := yaml.Marshal(manager["env"])
	if err != nil {
		t.Fatal(err)
	}
	if !omitEnv && !strings.Contains(string(data), "SOURCE_PROPAGATION") {
		t.Error("source environment variable did not propagate")
	}
	pod, err := yaml.Marshal(obj.Object)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(pod), "name: source-config") != 2 {
		t.Error("source volume and mount did not propagate exactly once each")
	}
	if !strings.Contains(string(pod), "source-env-settings") {
		t.Error("source envFrom did not propagate")
	}
	if strings.Contains(string(pod), "source.invalid") || strings.Contains(string(pod), "source-template") {
		t.Error("source connection flags bypassed named chart inputs")
	}
}

func TestFilterManagerArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
		err  bool
	}{
		{"empty", nil, []string{}, false},
		{"ordinary", []string{"--leader-elect", "--zap-log-level=debug"},
			[]string{"--leader-elect", "--zap-log-level=debug"}, false},
		{"inline references", []string{"--kubeconfig=/tmp/config", "--context=state",
			"--leader-election-namespace=state", "--nvke-controller-endpoint=http://source.invalid",
			"--nvke-controller-token-file=/tmp/token", "--infra-template-configmap=source"}, []string{}, false},
		{"split reference", []string{"--nvke-controller-endpoint", "http://source.invalid", "--leader-elect"},
			[]string{"--leader-elect"}, false},
		{"ordinary value", []string{"--namespace", "context", "--leader-elect"},
			[]string{"--namespace", "context", "--leader-elect"}, false},
		{"missing value", []string{"--context"}, nil, true},
		{"next flag", []string{"--context", "--leader-elect"}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := filterManagerArgs(tc.args)
			if (err != nil) != tc.err || !slices.Equal(got, tc.want) {
				t.Fatalf("filtered=%v error=%v; want %v error=%t", got, err, tc.want, tc.err)
			}
		})
	}
}
