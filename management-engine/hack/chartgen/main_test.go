// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// TestGenerationPropagatesSources exercises source edits through plugin generation and Helm rendering.
func TestGenerationPropagatesSources(t *testing.T) {
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
	changeCanonicalSources(t)
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
			containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
			manager := containers[0].(map[string]any)
			cpu, _, _ := unstructured.NestedString(manager, "resources", "requests", "cpu")
			delay, _, _ := unstructured.NestedInt64(manager, "readinessProbe", "initialDelaySeconds")
			data, err := yaml.Marshal(manager["env"])
			if err != nil {
				t.Fatal(err)
			}
			deployment = cpu == "25m" && delay == 37 && strings.Contains(string(data), "SOURCE_PROPAGATION")
			pod, err := yaml.Marshal(obj.Object)
			if err != nil {
				t.Fatal(err)
			}
			deployment = deployment && strings.Count(string(pod), "name: source-config") == 2

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

func changeCanonicalSources(t *testing.T) {
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
