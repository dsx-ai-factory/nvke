// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hack

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
)

func renderChart(t *testing.T, values string) ([]unstructured.Unstructured, string, error) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "values.json")
	if err := os.WriteFile(file, []byte(values), 0o600); err != nil {
		t.Fatal(err)
	}
	helm := os.Getenv("HELM")
	if helm == "" {
		helm = "helm"
	}
	cmd := exec.Command(helm, "template", "management-engine", "../chart", "--namespace", "runtime", "-f", file)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, string(output), err
	}
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	var resources []unstructured.Unstructured
	for {
		var object unstructured.Unstructured
		err := decoder.Decode(&object)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("invalid rendered YAML: %v", err)
		}
		if object.GetKind() != "" {
			resources = append(resources, object)
		}
	}
	return resources, string(output), nil
}

func objectsOfKind(resources []unstructured.Unstructured, kind string) []unstructured.Unstructured {
	var found []unstructured.Unstructured
	for _, r := range resources {
		if r.GetKind() == kind {
			found = append(found, r)
		}
	}
	return found
}

func TestChartModes(t *testing.T) {
	for _, tc := range []struct {
		name, values              string
		controller, state, remote bool
	}{
		{"combined", `{
  "manager": {
    "infrastructureTemplateConfigMap": "infra"
  }
}`, true, true, false},
		{"state only", `{
  "controller": {
    "enabled": false
  }
}`, false, true, false},
		{"legacy manager disabled", `{
  "manager": {
    "enabled": false
  }
}`, false, true, false},
		{"existing local state", `{
  "state": {
    "enabled": false
  },
  "manager": {
    "infrastructureTemplateConfigMap": "infra",
    "stateNamespace": "existing-state"
  }
}`, true, false, false},
		{"remote state", `{
  "state": {
    "enabled": false
  },
  "manager": {
    "infrastructureTemplateConfigMap": "infra",
    "stateNamespace": "remote-state",
    "stateAccess": {
      "mode": "remote",
      "kubeconfigSecret": {
        "name": "client",
        "key": "config"
      }
    }
  }
}`, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects, output, err := renderChart(t, tc.values)
			if err != nil {
				t.Fatalf("helm render: %v\n%s", err, output)
			}
			deployments := objectsOfKind(objects, "Deployment")
			wantDeployments := 0
			if tc.controller {
				wantDeployments = 1
			}
			if len(deployments) != wantDeployments {
				t.Fatalf("unexpected Deployments: %d", len(deployments))
			}
			crds := objectsOfKind(objects, "CustomResourceDefinition")
			wantCRDs := 0
			if tc.state {
				wantCRDs = 6
			}
			if len(crds) != wantCRDs {
				t.Fatalf("unexpected CRDs: %d", len(crds))
			}
			for _, crd := range crds {
				if crd.GetAnnotations()["helm.sh/resource-policy"] != "keep" {
					t.Fatal("CRD must be retained")
				}
			}
			if !tc.controller {
				for _, kind := range []string{"ServiceAccount", "Service", "NetworkPolicy", "ServiceMonitor", "Secret"} {
					if len(objectsOfKind(objects, kind)) != 0 {
						t.Fatalf("state-only install contains %s", kind)
					}
				}
				return
			}
			pod, _, _ := unstructured.NestedMap(deployments[0].Object, "spec", "template", "spec")
			if pod["automountServiceAccountToken"] != !tc.remote {
				t.Fatalf("wrong token setting: %v", pod)
			}
			containers := pod["containers"].([]any)
			manager := containers[0].(map[string]any)
			mounts, _, _ := unstructured.NestedSlice(manager, "volumeMounts")
			volumes, _, _ := unstructured.NestedSlice(pod, "volumes")
			if (len(mounts) == 1) != tc.remote || (len(volumes) == 1) != tc.remote {
				t.Fatalf("wrong kubeconfig mounts: %v %v", mounts, volumes)
			}
			args, _, _ := unstructured.NestedStringSlice(manager, "args")
			kubeconfigs := 0
			for _, arg := range args {
				if arg == "--kubeconfig=/etc/management-api/kubeconfig" {
					kubeconfigs++
				}
			}
			if (kubeconfigs == 1) != tc.remote {
				t.Fatalf("wrong kubeconfig arguments: %v", args)
			}
			if tc.remote {
				secret := volumes[0].(map[string]any)["secret"].(map[string]any)
				if secret["secretName"] != "client" {
					t.Fatalf("wrong Secret: %v", secret)
				}
				item := secret["items"].([]any)[0].(map[string]any)
				if item["key"] != "config" || item["path"] != "kubeconfig" {
					t.Fatalf("wrong Secret key mapping: %v", item)
				}
			}
			if tc.state {
				bindings := objectsOfKind(objects, "ClusterRoleBinding")
				if len(bindings) != 2 {
					t.Fatalf("expected manager and metrics bindings: %v", bindings)
				}
				for _, binding := range bindings {
					subjects, _, _ := unstructured.NestedSlice(binding.Object, "subjects")
					subject := subjects[0].(map[string]any)
					if subject["name"] != pod["serviceAccountName"] || subject["namespace"] != "runtime" {
						t.Fatalf("binding disagrees with Pod: %v", subject)
					}
				}
			} else if len(objectsOfKind(objects, "ClusterRoleBinding")) != 0 {
				t.Fatal("controller-only release grants datastore permissions locally")
			}
		})
	}
}

func TestChartStateConfiguration(t *testing.T) {
	values := `{
  "controller": {
    "enabled": false
  },
  "state": {
    "namespace": "state",
    "configMaps": [
      {
        "name": "infra",
        "namespace": "tenant-a",
        "data": {
          "clusterTmpl": "{{ .BootstrapFilesJSON }}"
        }
      }
    ],
    "rbac": {
      "subjects": [
        {
          "kind": "User",
          "apiGroup": "rbac.authorization.k8s.io",
          "name": "spiffe://example.org/manager"
        },
        {
          "kind": "ServiceAccount",
          "name": "manager",
          "namespace": "runtime"
        }
      ]
    }
  }
}`
	objects, output, err := renderChart(t, values)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	for _, kind := range []string{"ClusterRoleBinding", "RoleBinding"} {
		for _, binding := range objectsOfKind(objects, kind) {
			subjects, _, _ := unstructured.NestedSlice(binding.Object, "subjects")
			if len(subjects) != 2 || subjects[0].(map[string]any)["kind"] != "User" {
				t.Fatalf("lost external identity: %v", subjects)
			}
			if kind == "RoleBinding" && binding.GetNamespace() != "state" {
				t.Fatal("election binding in wrong namespace")
			}
		}
	}
	maps := objectsOfKind(objects, "ConfigMap")
	if len(maps) != 1 || maps[0].GetNamespace() != "tenant-a" {
		t.Fatalf("wrong ConfigMaps: %v", maps)
	}
	value, _, _ := unstructured.NestedString(maps[0].Object, "data", "clusterTmpl")
	if value != "{{ .BootstrapFilesJSON }}" {
		t.Fatalf("chart evaluated application template: %q", value)
	}
	objects, output, err = renderChart(t, `{
  "controller": {
    "enabled": false
  },
  "state": {
    "crds": {
      "install": false
    }
  }
}`)
	if err != nil || len(objectsOfKind(objects, "CustomResourceDefinition")) != 0 {
		t.Fatalf("external CRD owner ignored: %v %s", err, output)
	}
}

func TestChartValidation(t *testing.T) {
	for _, tc := range []struct{ values, want string }{
		{`{
  "controller": {
    "enabled": false
  },
  "state": {
    "enabled": false
  }
}`, "enable state.enabled or controller.enabled"},
		{`{
  "manager": {
    "infrastructureTemplateConfigMap": "infra",
    "stateAccess": {
      "mode": "remote"
    }
  }
}`, "remote state access requires state.enabled=false"},
		{`{
  "state": {
    "enabled": false
  },
  "manager": {
    "infrastructureTemplateConfigMap": "infra",
    "stateNamespace": "state",
    "stateAccess": {
      "mode": "remote"
    }
  }
}`, "kubeconfigSecret.name"},
		{`{
  "manager": {
    "stateAccess": {
      "mode": "invalid"
    }
  }
}`, "mode"},
		{`{
  "state": {
    "enabled": "false"
  }
}`, "boolean"},
		{`{}`, "infra-template-configmap"},
		{`{
  "manager": {
    "image": {
      "repository": ""
    }
  }
}`, "manager.image.repository"},
		{`{
  "manager": {
    "infrastructureTemplateConfigMap": "infra",
    "args": [
      "--kubeconfig=/other"
    ]
  }
}`, "manager.stateAccess"},
		{`{
  "manager": {
    "infrastructureTemplateConfigMap": "infra",
    "stateNamespace": "other"
  }
}`, "namespaces to agree"},
		{`{
  "manager": {
    "infrastructureTemplateConfigMap": "infra",
    "envOverrides": {
      "STATE_NAMESPACE": "other"
    }
  }
}`, "manager.stateNamespace"},
		{`{
  "controller": {
    "enabled": false
  },
  "state": {
    "rbac": {
      "subjects": [
        {
          "kind": "ServiceAccount",
          "name": "manager"
        }
      ]
    }
  }
}`, "namespace"},
	} {
		t.Run(fmt.Sprintf("%s/%s", tc.want, tc.values), func(t *testing.T) {
			_, output, err := renderChart(t, tc.values)
			if err == nil || !strings.Contains(output, tc.want) {
				t.Fatalf("want error containing %q, got %v\n%s", tc.want, err, output)
			}
		})
	}
}

func TestChartExamples(t *testing.T) {
	for _, sample := range []string{
		"runtime-values.yaml", "state-values.yaml", "remote-controller-values.yaml", "nvke-controller-values.yaml",
	} {
		t.Run(sample, func(t *testing.T) {
			helm := os.Getenv("HELM")
			if helm == "" {
				helm = "helm"
			}
			args := []string{
				"template", "management-engine", "../chart", "--namespace", "management-engine-system",
				"-f", "../config/samples/" + sample,
			}
			if sample == "nvke-controller-values.yaml" {
				args = append(args, "-f", "../config/samples/runtime-values.yaml")
			}
			output, err := exec.Command(helm, args...).CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, output)
			}
			if sample == "runtime-values.yaml" {
				decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
				found := false
				for {
					var object unstructured.Unstructured
					if err := decoder.Decode(&object); err == io.EOF {
						break
					} else if err != nil {
						t.Fatal(err)
					}
					if object.GetKind() == "ConfigMap" && object.GetName() == "infra-templates" {
						found = true
						for _, key := range []string{"clusterTmpl", "controlplaneTmpl", "nodepoolTmpl"} {
							value, _, _ := unstructured.NestedString(object.Object, "data", key)
							if value == "" {
								t.Fatalf("example missing %s", key)
							}
						}
					}
				}
				if !found {
					t.Fatal("example missing infra-templates ConfigMap")
				}
			}
		})
	}
}
