// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hack

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

func TestChartAPIModes(t *testing.T) {
	for _, tc := range []struct {
		name                                     string
		tenant, operator, state, manager, remote bool
	}{
		{"tenant only", true, false, false, false, false},
		{"operator only", false, true, false, false, false},
		{"both with state", true, true, true, false, false},
		{"all components", true, true, true, true, false},
		{"remote APIs", true, true, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := func() map[string]any {
				v := map[string]any{"enabled": true}
				if tc.remote {
					v["stateAccess"] = map[string]any{
						"mode": "remote", "context": "datastore",
						"kubeconfigSecret": map[string]any{"name": "api-client", "key": "config"},
					}
				}
				return v
			}
			values := map[string]any{
				"controller": map[string]any{"enabled": tc.manager},
				"manager":    map[string]any{"infrastructureTemplateConfigMap": "infra"},
				"state":      map[string]any{"enabled": tc.state},
				"metrics":    map[string]any{"enabled": false},
				"prometheus": map[string]any{"enabled": false},
			}
			if tc.tenant {
				v := api()
				v["namespace"] = "tenant-one"
				values["tenantAPI"] = v
			}
			if tc.operator {
				values["operatorAPI"] = api()
			}
			data, err := json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			objects, output, err := renderChart(t, string(data))
			if err != nil {
				t.Fatalf("render: %v\n%s", err, output)
			}
			count := 0
			if tc.tenant {
				count++
			}
			if tc.operator {
				count++
			}
			want := count
			if tc.manager {
				want++
			}
			if got := len(objectsOfKind(objects, "Deployment")); got != want {
				t.Fatalf("Deployments=%d want %d", got, want)
			}
			if got := len(objectsOfKind(objects, "Service")); got != count {
				t.Fatalf("Services=%d want %d", got, count)
			}
			identities := map[string]bool{}
			for _, surface := range []string{"tenant", "operator"} {
				enabled := (surface == "tenant" && tc.tenant) || (surface == "operator" && tc.operator)
				if !enabled {
					continue
				}
				name := "cluster-service-" + surface + "-api"
				deployment := apiObject(t, objects, "Deployment", name)
				sa := checkAPIPod(t, objects, deployment, surface, tc.remote)
				if identities[sa] {
					t.Fatal("APIs share an identity")
				}
				identities[sa] = true
				checkAPIService(t, objects, deployment)
			}
			if tc.state && tc.tenant {
				role := apiObject(t, objects, "Role", "cluster-service-tenant-api")
				if role.GetNamespace() != "tenant-one" {
					t.Fatal(role.GetNamespace())
				}
				apiObject(t, objects, "ClusterRoleBinding", "cluster-service-tenant-catalog")
				binding := apiObject(t, objects, "RoleBinding", "cluster-service-tenant-api")
				subjects := binding.Object["subjects"].([]any)
				if len(subjects) != 1 || subjects[0].(map[string]any)["name"] != "cluster-service-tenant-api" {
					t.Fatal(subjects)
				}
			}
			if tc.state && tc.operator {
				apiObject(t, objects, "ClusterRoleBinding", "cluster-service-operator-api")
			}
			for _, kind := range []string{"Secret", "HTTPRoute", "Gateway"} {
				if len(objectsOfKind(objects, kind)) != 0 {
					t.Fatalf("unexpected %s", kind)
				}
			}
		})
	}
}

func apiObject(t *testing.T, objects []unstructured.Unstructured, kind, name string) unstructured.Unstructured {
	t.Helper()
	for _, o := range objects {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	t.Fatalf("missing %s %s", kind, name)
	return unstructured.Unstructured{}
}

func TestChartAPIValidation(t *testing.T) {
	for _, tc := range []struct{ name, values, want string }{
		{"missing tenant", `{"tenantAPI":{"enabled":true}}`, "tenantAPI.namespace is required"},
		{"invalid tenant", `{"tenantAPI":{"enabled":true,"namespace":"Bad_NS"}}`, "namespace"},
		{"missing image",
			`{"operatorAPI":{"enabled":true,"image":{"repository":""}}}`,
			"operatorAPI.image.repository is required"},
		{"remote secret", `{"operatorAPI":{"enabled":true,"stateAccess":{"mode":"remote"}}}`, "kubeconfigSecret.name"},
		{"remote key", `{
  "operatorAPI": {
    "enabled": true,
    "stateAccess": {
      "mode": "remote",
      "kubeconfigSecret": {
        "name": "client",
        "key": ""
      }
    }
  }
}`, "kubeconfigSecret.key"},
		{"local credentials",
			`{"operatorAPI":{"enabled":true,"stateAccess":{"kubeconfigSecret":{"name":"client"}}}}`,
			"inCluster access"},
		{"local context", `{"operatorAPI":{"enabled":true,"stateAccess":{"context":"elsewhere"}}}`, "inCluster access"},
		{"invalid mode", `{"operatorAPI":{"enabled":true,"stateAccess":{"mode":"other"}}}`, "mode"},
		{"existing identity", `{"operatorAPI":{"enabled":true,"serviceAccount":{"create":false}}}`, "serviceAccount.name"},
		{"invalid port", `{"operatorAPI":{"enabled":true,"service":{"port":0}}}`, "port"},
		{"invalid replicas", `{"operatorAPI":{"enabled":true,"replicas":0}}`, "replicas"},
		{"protected flag", `{"operatorAPI":{"enabled":true,"args":["--kubeconfig=other"]}}`, "named chart values"},
		{"protected split flag", `{
  "tenantAPI": {
    "enabled": true,
    "namespace": "tenant-one",
    "args": [
      "--namespace",
      "other"
    ]
  }
}`, "named chart values"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var values map[string]any
			if err := json.Unmarshal([]byte(tc.values), &values); err != nil {
				t.Fatal(err)
			}
			values["controller"] = map[string]any{"enabled": false}
			data, err := json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			_, out, err := renderChart(t, string(data))
			if err == nil || !strings.Contains(out, tc.want) {
				t.Fatalf("expected %q error: %v\n%s", tc.want, err, out)
			}
		})
	}
}

func TestChartAPIOverrides(t *testing.T) {
	objects, out, err := renderChart(t, `{
  "controller": {
    "enabled": false
  },
  "state": {
    "enabled": false
  },
  "operatorAPI": {
    "enabled": true,
    "image": {
      "repository": "example.invalid/operator@sha256:abc"
    },
    "replicas": 2,
    "resources": {
      "requests": {
        "cpu": "75m"
      }
    },
    "nodeSelector": {
      "pool": "api"
    },
    "readinessProbe": {
      "initialDelaySeconds": 19
    },
    "service": {
      "port": 8181
    },
    "serviceAccount": {
      "create": false,
      "name": "existing-api"
    },
    "extraVolumes": [
      {
        "name": "settings",
        "configMap": {
          "name": "api-settings"
        }
      }
    ],
    "extraVolumeMounts": [
      {
        "name": "settings",
        "mountPath": "/etc/settings"
      }
    ]
  }
}`)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	d := apiObject(t, objects, "Deployment", "cluster-service-operator-api")
	c, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	container := c[0].(map[string]any)
	if container["image"] != "example.invalid/operator@sha256:abc" {
		t.Fatal(container["image"])
	}
	cpu, _, _ := unstructured.NestedString(container, "resources", "requests", "cpu")
	if cpu != "75m" {
		t.Fatal(cpu)
	}
	delay, _, _ := unstructured.NestedInt64(container, "readinessProbe", "initialDelaySeconds")
	if delay != 19 {
		t.Fatal(delay)
	}
	replicas, _, _ := unstructured.NestedInt64(d.Object, "spec", "replicas")
	if replicas != 2 {
		t.Fatal(replicas)
	}
	pool, _, _ := unstructured.NestedString(d.Object, "spec", "template", "spec", "nodeSelector", "pool")
	if pool != "api" {
		t.Fatal(pool)
	}
	if len(objectsOfKind(objects, "ServiceAccount")) != 0 {
		t.Fatal("created existing identity")
	}
	if len(container["volumeMounts"].([]any)) != 1 {
		t.Fatal(container)
	}
	service := apiObject(t, objects, "Service", "cluster-service-operator-api")
	p, _, _ := unstructured.NestedSlice(service.Object, "spec", "ports")
	if p[0].(map[string]any)["port"] != int64(8181) {
		t.Fatal(p)
	}
}

func checkAPIPod(
	t *testing.T,
	objects []unstructured.Unstructured,
	deployment unstructured.Unstructured,
	surface string,
	remote bool,
) string {
	t.Helper()
	pod, _, _ := unstructured.NestedMap(deployment.Object, "spec", "template", "spec")
	sa := pod["serviceAccountName"].(string)
	apiObject(t, objects, "ServiceAccount", sa)
	if pod["automountServiceAccountToken"] != !remote {
		t.Fatalf("token mount: %v", pod)
	}
	containers := pod["containers"].([]any)
	c := containers[0].(map[string]any)
	args := fmt.Sprint(c["args"])
	if !strings.Contains(args, "--listen=:8080") {
		t.Fatal(args)
	}
	if surface == "tenant" && !strings.Contains(args, "--namespace=tenant-one") {
		t.Fatal(args)
	}
	if strings.Contains(args, "tenant-test") {
		t.Fatal("development tenant leaked")
	}
	volumes, _, _ := unstructured.NestedSlice(pod, "volumes")
	mounts, _, _ := unstructured.NestedSlice(c, "volumeMounts")
	if remote {
		contextSet := strings.Contains(args, "--context=datastore")
		if !contextSet || !strings.Contains(args, "--kubeconfig=/etc/cluster-service/kubeconfig") {
			t.Fatal(args)
		}
		if len(volumes) != 1 || len(mounts) != 1 {
			t.Fatalf("remote volume/mount: %v %v", volumes, mounts)
		}
		if mounts[0].(map[string]any)["readOnly"] != true {
			t.Fatal("writable kubeconfig")
		}
		secret := volumes[0].(map[string]any)["secret"].(map[string]any)
		if secret["secretName"] != "api-client" || secret["items"].([]any)[0].(map[string]any)["key"] != "config" {
			t.Fatal(secret)
		}
	} else if len(volumes) != 0 || len(mounts) != 0 {
		t.Fatal("local API needs no credential Secret")
	}

	return sa
}

func checkAPIService(t *testing.T, objects []unstructured.Unstructured, deployment unstructured.Unstructured) {
	t.Helper()
	name := deployment.GetName()
	service := apiObject(t, objects, "Service", name)
	selector, _, _ := unstructured.NestedStringMap(service.Object, "spec", "selector")
	labels, _, _ := unstructured.NestedStringMap(deployment.Object, "spec", "template", "metadata", "labels")
	ds, _, _ := unstructured.NestedStringMap(deployment.Object, "spec", "selector", "matchLabels")
	if !reflect.DeepEqual(selector, ds) {
		t.Fatalf("Service selector differs: %v %v", selector, ds)
	}
	for k, v := range selector {
		if labels[k] != v {
			t.Fatal("selector does not match pod")
		}
	}
	for _, other := range objectsOfKind(objects, "Deployment") {
		if other.GetName() == name {
			continue
		}
		ol, _, _ := unstructured.NestedStringMap(other.Object, "spec", "template", "metadata", "labels")
		matches := true
		for k, v := range selector {
			matches = matches && ol[k] == v
		}
		if matches {
			t.Fatal("Service selects another workload")
		}
	}
	ports, _, _ := unstructured.NestedSlice(service.Object, "spec", "ports")
	p := ports[0].(map[string]any)
	if p["name"] != "http" || p["targetPort"] != "http" || p["port"] != int64(8080) {
		t.Fatal(p)
	}
	typ, _, _ := unstructured.NestedString(service.Object, "spec", "type")
	if typ != "ClusterIP" {
		t.Fatal(typ)
	}
}

func TestChartAPIReleaseIsolation(t *testing.T) {
	values := `{"controller":{"enabled":false},"operatorAPI":{"enabled":true}}`
	first, out, err := renderChartRelease(t, "first", values)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	second, out, err := renderChartRelease(t, "second", values)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	service := apiObject(t, first, "Service", "first-cluster-service-operator-api")
	pod := apiObject(t, second, "Deployment", "second-cluster-service-operator-api")
	selector, _, _ := unstructured.NestedStringMap(service.Object, "spec", "selector")
	labels, _, _ := unstructured.NestedStringMap(pod.Object, "spec", "template", "metadata", "labels")
	if selector["app.kubernetes.io/instance"] == labels["app.kubernetes.io/instance"] {
		t.Fatal("Service selects another release")
	}
}

func TestChartAPIExamples(t *testing.T) {
	for _, name := range []string{"all-components", "remote-apis"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile("../config/samples/" + name + "-values.yaml")
			if err != nil {
				t.Fatal(err)
			}
			data, err = yaml.YAMLToJSON(data)
			if err != nil {
				t.Fatal(err)
			}
			objects, out, err := renderChart(t, string(data))
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			want := 2
			if name == "all-components" {
				want = 3
			}
			if len(objectsOfKind(objects, "Deployment")) != want {
				t.Fatal("example omitted workloads")
			}
		})
	}
}

func TestChartAPIBindingMerge(t *testing.T) {
	objects, out, err := renderChart(t, `{
 "controller":{"enabled":false},
 "tenantAPI":{"enabled":true,"namespace":"tenant-one"},
 "state":{"rbac":{"tenantAPIBindings":[{"namespace":"tenant-one","subjects":[
 {"kind":"ServiceAccount","name":"existing","namespace":"other"}
 ]}]}}
 }`)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	binding := apiObject(t, objects, "RoleBinding", "cluster-service-tenant-api")
	if len(binding.Object["subjects"].([]any)) != 2 {
		t.Fatal(binding.Object["subjects"])
	}
	count := 0
	for _, role := range objectsOfKind(objects, "Role") {
		if role.GetName() == "cluster-service-tenant-api" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("duplicate tenant Roles: %d", count)
	}
}

func TestChartAPILongNamesAndDistinctIdentities(t *testing.T) {
	values := map[string]any{
		"controller":       map[string]any{"enabled": false},
		"nameOverride":     strings.Repeat("a", 63),
		"fullnameOverride": strings.Repeat("b", 63),
		"tenantAPI":        map[string]any{"enabled": true, "namespace": "123"},
		"operatorAPI":      map[string]any{"enabled": true},
	}
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	objects, out, err := renderChart(t, string(data))
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, obj := range objectsOfKind(objects, "Deployment") {
		if len(obj.GetName()) > 63 {
			t.Fatal(obj.GetName())
		}
		labels, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "labels")
		for _, value := range labels {
			if errors := validation.IsValidLabelValue(value); len(errors) > 0 {
				t.Fatal(errors)
			}
		}
		checkAPIService(t, objects, obj)
	}
	shared := map[string]any{
		"enabled": true, "namespace": "tenant-one",
		"serviceAccount": map[string]any{"name": "shared"},
	}
	values = map[string]any{"controller": map[string]any{"enabled": false}, "tenantAPI": shared, "operatorAPI": shared}
	data, err = json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	_, out, err = renderChart(t, string(data))
	if err == nil || !strings.Contains(out, "distinct ServiceAccounts") {
		t.Fatalf("%v: %s", err, out)
	}
}
