// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hack

import (
	"bytes"
	"encoding/json"
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
	var input map[string]any
	if err := json.Unmarshal([]byte(values), &input); err != nil {
		t.Fatal(err)
	}
	manager, ok := input["manager"].(map[string]any)
	if input["manager"] == nil {
		manager = map[string]any{}
		input["manager"] = manager
		ok = true
	}
	if ok && manager["nvkeController"] == nil {
		manager["nvkeController"] = map[string]any{"endpoint": "https://backend.example.invalid/api"}
	}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "values.json")
	if err := os.WriteFile(file, data, 0o600); err != nil {
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
  },
  "manager": {
    "image": {
      "repository": ""
    },
    "nvkeController": {
      "endpoint": "",
      "tokenSecret": {
        "name": "",
        "key": ""
      }
    }
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
				wantCRDs = 3
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

func TestChartMetricsAuthenticationPermissions(t *testing.T) {
	for _, tc := range []struct {
		name, values string
		wantAuth     bool
	}{
		{"combined secure metrics", `{"manager":{"infrastructureTemplateConfigMap":"infra"}}`, true},
		{"combined metrics disabled", `{
  "manager":{"infrastructureTemplateConfigMap":"infra"}, "metrics":{"enabled":false}
}`, false},
		{"combined insecure metrics", `{
  "manager":{"infrastructureTemplateConfigMap":"infra"}, "metrics":{"secure":false}
}`, false},
		{"state only", `{"controller":{"enabled":false}}`, false},
		{"manager disabled", `{"manager":{"enabled":false}}`, false},
		{"state explicit authentication", `{
  "controller":{"enabled":false},
  "state":{"rbac":{
    "metricsAuth":true,
    "subjects":[{"kind":"User","name":"remote-manager","apiGroup":"rbac.authorization.k8s.io"}]
  }}
}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects, output, err := renderChart(t, tc.values)
			if err != nil {
				t.Fatalf("render: %v\n%s", err, output)
			}
			found := false
			for _, role := range objectsOfKind(objects, "ClusterRole") {
				rules, _, _ := unstructured.NestedSlice(role.Object, "rules")
				for _, raw := range rules {
					rule := raw.(map[string]any)
					resources, _, _ := unstructured.NestedStringSlice(rule, "resources")
					for _, resource := range resources {
						if resource == "tokenreviews" || resource == "subjectaccessreviews" {
							found = true
						}
					}
				}
			}
			if found != tc.wantAuth {
				t.Fatalf("metrics authentication permissions present=%t, want %t", found, tc.wantAuth)
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
  "nvke-crds": {
    "enabled": false
  },
  "state": {"crds":{"install":false}}
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
		{`{}`, "manager.infrastructureTemplateConfigMap"},
		{`{"manager":{"infrastructureTemplateConfigMap":"invalid/name"}}`, "infrastructureTemplateConfigMap"},
		{`{
  "controller": {
    "enabled": false
  },
  "state": {
    "rbac": {
      "subjects": [
        {
          "kind": "ServiceAccount",
          "name": "manager",
          "namespace": "runtime",
          "apiGroup": "invalid"
        }
      ]
    }
  }
}`, "apiGroup"},
		{`{"manager":{"nvkeController":{"endpoint":"https:///api"}}}`, "must include a host"},
		{`{
  "state": {
    "enabled": false
  },
  "manager": {
    "infrastructureTemplateConfigMap": "infra",
    "stateNamespace": "state",
    "stateAccess": {
      "mode": "remote",
      "kubeconfigSecret": {
        "name": "client",
        "key": "/config"
      }
    }
  }
}`, "key"},
		{`{
  "controller": {
    "enabled": false
  },
  "state": {
    "rbac": {
      "tenantAPIBindings": [
        {
          "namespace": "tenant-a",
          "subjects": [
            {
              "kind": "ServiceAccount",
              "name": "first",
              "namespace": "runtime"
            }
          ]
        },
        {
          "namespace": "tenant-a",
          "subjects": [
            {
              "kind": "ServiceAccount",
              "name": "second",
              "namespace": "runtime"
            }
          ]
        }
      ]
    }
  }
}`, "one state.rbac.tenantAPIBindings entry per namespace"},
		{`{"manager":{"nvkeController":{"endpoint":""}}}`, "manager.nvkeController.endpoint"},
		{`{"manager":{"nvkeController":{"endpoint":"invalid"}}}`, "endpoint"},
		{`{
  "manager": {
    "infrastructureTemplateConfigMap": "infra",
    "nvkeController": {
      "endpoint": "https://backend.example.invalid/api",
      "tokenSecret": {
        "name": "client",
        "key": ""
      }
    }
  }
}`, "tokenSecret.key"},
		{`{
  "manager": {
    "infrastructureTemplateConfigMap": "infra",
    "nvkeController": {
      "endpoint": "https://backend.example.invalid/api",
      "tokenSecret": {
        "name": "client",
        "key": "/token"
      }
    }
  }
}`, "key"},
		{`{
  "manager": {
    "infrastructureTemplateConfigMap": "infra",
    "args": [
      "--leader-elect",
      "--leader-election-namespace=wrong"
    ]
  }
}`, "the chart sets --leader-election-namespace"},
		{`{
  "manager": {
    "infrastructureTemplateConfigMap": "infra",
    "args": [
      "--nvke-controller-endpoint=https://wrong.example.invalid"
    ]
  }
}`, "manager.nvkeController"},
		{`{
  "controller": {
    "enabled": false
  },
  "state": {
    "rbac": {
      "tenantAPIBindings": [
        {
          "subjects": [
            {
              "kind": "ServiceAccount",
              "name": "api",
              "namespace": "runtime"
            }
          ]
        }
      ]
    }
  }
}`, "namespace"},
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
		"runtime-values.yaml", "state-values.yaml", "remote-controller-values.yaml",
		"nvke-controller-values.yaml", "dsx-values.yaml",
	} {
		t.Run(sample, func(t *testing.T) {
			helm := os.Getenv("HELM")
			if helm == "" {
				helm = "helm"
			}
			args := []string{"template", "management-engine", "../chart", "--namespace", "management-engine-system"}
			if sample == "nvke-controller-values.yaml" || sample == "dsx-values.yaml" {
				args = append(args, "-f", "../config/samples/runtime-values.yaml")
			}
			args = append(args, "-f", "../config/samples/"+sample)
			output, err := exec.Command(helm, args...).CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, output)
			}
			if sample == "state-values.yaml" || sample == "remote-controller-values.yaml" {
				decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
				crds := map[string]bool{}
				for {
					var object unstructured.Unstructured
					if err := decoder.Decode(&object); err == io.EOF {
						break
					} else if err != nil {
						t.Fatal(err)
					}
					if sample == "state-values.yaml" && object.GetKind() == "CustomResourceDefinition" {
						crds[object.GetName()] = true
					}
					if sample == "remote-controller-values.yaml" {
						if object.GetKind() == "Service" || object.GetKind() == "ServiceMonitor" {
							t.Fatalf("remote example unexpectedly enables metrics: %s", object.GetKind())
						}
						if object.GetKind() == "Deployment" {
							containers, _, _ := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
							args, _, _ := unstructured.NestedStringSlice(containers[0].(map[string]any), "args")
							if !strings.Contains(strings.Join(args, " "), "--metrics-bind-address=0") {
								t.Fatal("remote example must disable the metrics listener")
							}
						}
					}
				}
				if sample == "state-values.yaml" {
					for _, kind := range []string{"machineimages", "clusterreleases", "nodepoolreleases"} {
						if !crds[kind+".internal.nvke.nvidia.com"] {
							t.Fatalf("standalone state example missing catalog CRD %s", kind)
						}
					}
				}
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

func TestChartBackendCredentials(t *testing.T) {
	for _, mode := range []string{"inCluster", "remote"} {
		t.Run(mode, func(t *testing.T) {
			values := fmt.Sprintf(`{
     "state":{"enabled":false},
     "manager":{
       "infrastructureTemplateConfigMap":"infra",
       "stateNamespace":"state",
       "args":["--leader-elect"],
       "stateAccess":{"mode":%q,"kubeconfigSecret":{"name":"state-client","key":"config"}},
       "nvkeController":{
         "endpoint":"https://backend.example.invalid/api",
         "tokenSecret":{"name":"backend-client","key":"bearer"}
       }
     }
   }`, mode)
			objects, output, err := renderChart(t, values)
			if err != nil {
				t.Fatalf("%v\n%s", err, output)
			}
			pod, _, _ := unstructured.NestedMap(objectsOfKind(objects, "Deployment")[0].Object, "spec", "template", "spec")
			container := pod["containers"].([]any)[0].(map[string]any)
			args, _, _ := unstructured.NestedStringSlice(container, "args")
			for _, wanted := range []string{
				"--nvke-controller-endpoint=https://backend.example.invalid/api",
				"--nvke-controller-token-file=/etc/nvke-controller/auth/token",
				"--leader-election-namespace=$(STATE_NAMESPACE)",
			} {
				count := 0
				for _, arg := range args {
					if arg == wanted {
						count++
					}
				}
				if count != 1 {
					t.Fatalf("want one %q, got %v", wanted, args)
				}
			}
			volumes, _, _ := unstructured.NestedSlice(pod, "volumes")
			found := false
			for _, volume := range volumes {
				v := volume.(map[string]any)
				if v["name"] != "nvke-controller-token" {
					continue
				}
				found = true
				secret := v["secret"].(map[string]any)
				item := secret["items"].([]any)[0].(map[string]any)
				if secret["secretName"] != "backend-client" || item["key"] != "bearer" || item["path"] != "token" {
					t.Fatalf("wrong backend credential reference: %v", secret)
				}
			}
			if !found {
				t.Fatal("missing backend credentials volume")
			}
			mounts, _, _ := unstructured.NestedSlice(container, "volumeMounts")
			found = false
			for _, mount := range mounts {
				m := mount.(map[string]any)
				if m["name"] == "nvke-controller-token" {
					found = m["mountPath"] == "/etc/nvke-controller/auth" && m["readOnly"] == true
				}
			}
			if !found {
				t.Fatal("missing read-only backend credentials mount")
			}
		})
	}
}

func TestChartRemoteNamespaceAlias(t *testing.T) {
	objects, output, err := renderChart(t, `{
  "state": {
    "enabled": false,
    "namespace": "existing-state"
  },
  "manager": {
    "infrastructureTemplateConfigMap": "infra",
    "stateAccess": {
      "mode": "remote",
      "kubeconfigSecret": {
        "name": "client",
        "key": "config"
      }
    }
  }
}`)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	env, _, _ := unstructured.NestedSlice(objectsOfKind(objects, "Deployment")[0].Object,
		"spec", "template", "spec", "containers")
	container := env[0].(map[string]any)
	variables, _, _ := unstructured.NestedSlice(container, "env")
	if variables[0].(map[string]any)["value"] != "existing-state" {
		t.Fatalf("wrong state namespace: %v", variables)
	}
}

func TestChartAPIBindings(t *testing.T) {
	objects, output, err := renderChart(t, `{
  "controller": {
    "enabled": false
  },
  "state": {
    "rbac": {
      "tenantAPIBindings": [
        {
          "namespace": "tenant-a",
          "subjects": [
            {
              "kind": "ServiceAccount",
              "name": "tenant-api",
              "namespace": "runtime"
            }
          ]
        }
      ],
      "operatorAPISubjects": [
        {
          "kind": "User",
          "apiGroup": "rbac.authorization.k8s.io",
          "name": "spiffe://example.org/operator-api"
        }
      ]
    }
  }
}`)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if len(objectsOfKind(objects, "Deployment")) != 0 || len(objectsOfKind(objects, "ServiceAccount")) != 0 {
		t.Fatal("state bindings installed workloads or runtime identities")
	}
	var tenantRole, operatorRole bool
	for _, role := range append(objectsOfKind(objects, "Role"), objectsOfKind(objects, "ClusterRole")...) {
		if !strings.HasSuffix(role.GetName(), "-api") {
			continue
		}
		rules, _, _ := unstructured.NestedSlice(role.Object, "rules")
		if len(rules) != 1 {
			t.Fatalf("unexpected API rules: %v", rules)
		}
		resources, _, _ := unstructured.NestedStringSlice(rules[0].(map[string]any), "resources")
		if strings.HasSuffix(role.GetName(), "tenant-api") {
			tenantRole = role.GetNamespace() == "tenant-a" && len(resources) == 1 && resources[0] == "nvkeclusters"
		} else {
			wantResources := "clusterreleases,machineimages,controlplaneclasses,clusterreleasechannels,nodepoolreleases"
			operatorRole = role.GetKind() == "ClusterRole" && strings.Join(resources, ",") == wantResources
		}
	}
	if len(objectsOfKind(objects, "RoleBinding")) != 1 || len(objectsOfKind(objects, "ClusterRoleBinding")) != 1 {
		t.Fatal("missing or excessive API bindings")
	}
	if !tenantRole || !operatorRole {
		t.Fatal("API permissions do not match existing registrations")
	}
	for _, kind := range []string{"RoleBinding", "ClusterRoleBinding"} {
		for _, binding := range objectsOfKind(objects, kind) {
			if !strings.HasSuffix(binding.GetName(), "-api") {
				continue
			}
			subjects, _, _ := unstructured.NestedSlice(binding.Object, "subjects")
			subject := subjects[0].(map[string]any)
			if strings.HasSuffix(binding.GetName(), "tenant-api") {
				if binding.GetNamespace() != "tenant-a" ||
					subject["kind"] != "ServiceAccount" || subject["namespace"] != "runtime" {
					t.Fatalf("wrong tenant binding: %v", binding)
				}
			} else if subject["kind"] != "User" || subject["name"] != "spiffe://example.org/operator-api" {
				t.Fatalf("wrong operator binding: %v", binding)
			}
		}
	}
}

func TestChartImages(t *testing.T) {
	for _, tc := range []struct{ repository, tag, want string }{
		{"registry.example.invalid/manager", "reviewed", "registry.example.invalid/manager:reviewed"},
		{"registry.example.invalid/manager@sha256:" + strings.Repeat("a", 64), "ignored",
			"registry.example.invalid/manager@sha256:" + strings.Repeat("a", 64)},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			values := fmt.Sprintf(`{
    "manager":{"infrastructureTemplateConfigMap":"infra","image":{"repository":%q,"tag":%q}}
   }`, tc.repository, tc.tag)
			objects, output, err := renderChart(t, values)
			if err != nil {
				t.Fatalf("%v\n%s", err, output)
			}
			containers, _, _ := unstructured.NestedSlice(objectsOfKind(objects, "Deployment")[0].Object,
				"spec", "template", "spec", "containers")
			if containers[0].(map[string]any)["image"] != tc.want {
				t.Fatalf("wrong image: %v", containers[0])
			}
		})
	}
}

func TestChartExistingManagerIdentity(t *testing.T) {
	objects, output, err := renderChart(t, `{
  "serviceAccount": {
    "enabled": false,
    "name": "existing-manager"
  },
  "manager": {
    "infrastructureTemplateConfigMap": "infra"
  }
}`)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if len(objectsOfKind(objects, "ServiceAccount")) != 0 {
		t.Fatal("existing identity was recreated")
	}
	deployment := objectsOfKind(objects, "Deployment")[0]
	name, _, _ := unstructured.NestedString(deployment.Object, "spec", "template", "spec", "serviceAccountName")
	if name != "existing-manager" {
		t.Fatalf("wrong runtime identity: %s", name)
	}
	for _, binding := range objectsOfKind(objects, "ClusterRoleBinding") {
		subjects, _, _ := unstructured.NestedSlice(binding.Object, "subjects")
		subject := subjects[0].(map[string]any)
		if subject["name"] != "existing-manager" || subject["namespace"] != "runtime" {
			t.Fatalf("state grants do not match existing runtime identity: %v", subject)
		}
	}
}

func TestChartOwnedFlags(t *testing.T) {
	for _, name := range []string{
		"leader-election-namespace", "nvke-controller-endpoint", "nvke-controller-token-file",
		"infra-template-configmap", "kubeconfig", "context",
	} {
		for _, prefix := range []string{"-", "--"} {
			t.Run(prefix+name, func(t *testing.T) {
				values := fmt.Sprintf(`{
      "manager":{"infrastructureTemplateConfigMap":"infra","args":[%q]}
     }`, prefix+name+"=override")
				_, output, err := renderChart(t, values)
				if err == nil || !strings.Contains(output, "manager.") {
					t.Fatalf("protected argument accepted: %v\n%s", err, output)
				}
			})
		}
	}
}

func TestChartEndpointValidation(t *testing.T) {
	for _, endpoint := range []string{
		"http://:8080/api", "https://@backend.example.invalid/api",
		"https://backend.example.invalid/api?key=placeholder", "https://backend.example.invalid/api?",
		"https://backend.example.invalid/api#fragment",
	} {
		t.Run(endpoint, func(t *testing.T) {
			values := fmt.Sprintf(`{
    "manager":{"infrastructureTemplateConfigMap":"infra","nvkeController":{"endpoint":%q}}
   }`, endpoint)
			_, output, err := renderChart(t, values)
			if err == nil || !strings.Contains(output, "manager.nvkeController") {
				t.Fatalf("invalid backend endpoint accepted: %v\n%s", err, output)
			}
		})
	}
}

func TestChartCatalogOwnership(t *testing.T) {
	catalog := map[string]bool{
		"machineimages.internal.nvke.nvidia.com":    true,
		"clusterreleases.internal.nvke.nvidia.com":  true,
		"nodepoolreleases.internal.nvke.nvidia.com": true,
	}
	for _, includeCatalog := range []bool{false, true} {
		values := fmt.Sprintf(`{"controller":{"enabled":false},"nvke-crds":{"enabled":%t}}`, includeCatalog)
		objects, output, err := renderChart(t, values)
		if err != nil {
			t.Fatalf("%v\n%s", err, output)
		}
		seen := map[string]bool{}
		catalogCount, localCount := 0, 0
		for _, crd := range objectsOfKind(objects, "CustomResourceDefinition") {
			name := crd.GetName()
			if seen[name] {
				t.Fatalf("duplicate CRD %s", name)
			}
			seen[name] = true
			if catalog[name] {
				catalogCount++
			} else {
				localCount++
			}
		}
		wantCatalog := 0
		if includeCatalog {
			wantCatalog = 3
		}
		if catalogCount != wantCatalog || localCount != 3 {
			t.Fatalf("catalog enabled=%t: got %d catalog and %d local CRDs", includeCatalog, catalogCount, localCount)
		}
	}
}
