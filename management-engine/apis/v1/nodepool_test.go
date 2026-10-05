// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

func clusterSample(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("../../config/samples/internal_v1_nkecluster.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Strict typed decoding catches sample fields the API would otherwise prune.
	var cluster NKECluster
	if err := yaml.UnmarshalStrict(data, &cluster); err != nil {
		t.Fatal(err)
	}
	return decodeJSON[map[string]any](t, encodeJSON(t, cluster))
}

func TestNodePoolAdmission(t *testing.T) {
	schema := loadResourceSchema(t, "nkeclusters")
	sample := clusterSample(t)
	schema.admit(t, sample, nil)

	for _, tt := range []struct {
		name    string
		change  func(map[string]any)
		invalid bool
	}{
		{name: "zero replicas", change: func(p map[string]any) { p["replicas"] = int64(0) }},
		{name: "missing replicas", change: func(p map[string]any) { delete(p, "replicas") }, invalid: true},
		{name: "null replicas", change: func(p map[string]any) { p["replicas"] = nil }, invalid: true},
		{name: "negative replicas", change: func(p map[string]any) { p["replicas"] = int64(-1) }, invalid: true},
		{name: "missing pool name", change: func(p map[string]any) { delete(p, "name") }, invalid: true},
		{name: "invalid pool name", change: func(p map[string]any) { p["name"] = "GPU Pool" }, invalid: true},
		{name: "missing subnet", change: func(p map[string]any) { delete(p, "vpcSubnetID") }, invalid: true},
		{name: "empty subnet", change: func(p map[string]any) { p["vpcSubnetID"] = "" }, invalid: true},
		{name: "invalid subnet ID", change: func(p map[string]any) { p["vpcSubnetID"] = "not-a-uuid" }, invalid: true},
		{name: "missing profile", change: func(p map[string]any) { delete(p, "nodeProfile") }, invalid: true},
		{name: "unversioned profile", change: func(p map[string]any) { p["nodeProfile"] = "ubuntu-24.04" }, invalid: true},
		{name: "empty profile tag", change: func(p map[string]any) { p["nodeProfile"] = "ubuntu-24.04:" }, invalid: true},
		{name: "invalid profile name", change: func(p map[string]any) { p["nodeProfile"] = "bad name:v1" }, invalid: true},
		{name: "reserved label", change: func(p map[string]any) {
			setField(p, []string{"userParams", "labels"}, map[string]any{"kubernetes.io/role": "worker"})
		}, invalid: true},
		{name: "invalid taint effect", change: func(p map[string]any) {
			setField(p, []string{"userParams", "taints"}, []any{map[string]any{"key": "workload", "effect": "Invalid"}})
		}, invalid: true},
		{name: "duplicate taint", change: func(p map[string]any) {
			taint := map[string]any{"key": "workload", "effect": "NoSchedule"}
			setField(p, []string{"userParams", "taints"}, []any{taint, taint})
		}, invalid: true},
		{name: "same taint key with distinct effects", change: func(p map[string]any) {
			setField(p, []string{"userParams", "taints"}, []any{
				map[string]any{"key": "workload", "effect": "NoSchedule"},
				map[string]any{"key": "workload", "effect": "NoExecute"},
			})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			obj := runtime.DeepCopyJSON(sample)
			pool := specOf(t, obj)["nodePools"].([]any)[0].(map[string]any)
			tt.change(pool)
			schema.check(t, tt.invalid, obj, nil)
		})
	}

	schema.runUpdate(t, sample, []fieldCase{
		{name: "missing network", path: []string{"spec", "network"}, remove: true, invalid: true},
		{name: "missing VPC ID", path: []string{"spec", "network", "vpcID"}, remove: true, invalid: true},
		{name: "invalid VPC ID", path: []string{"spec", "network", "vpcID"}, value: "not-a-uuid", invalid: true},
		{name: "remove pools", path: []string{"spec", "nodePools"}, remove: true},
	})
	t.Run("cluster subnet cannot substitute for pool subnet", func(t *testing.T) {
		obj := runtime.DeepCopyJSON(sample)
		pool := specOf(t, obj)["nodePools"].([]any)[0].(map[string]any)
		setField(obj, []string{"spec", "network", "vpcSubnetID"}, pool["vpcSubnetID"])
		delete(pool, "vpcSubnetID")
		schema.check(t, true, obj, nil)
	})
	for _, name := range []string{"runtime", "rollout"} {
		if _, found := specSchema(t, "nkeclusters").Properties[name]; found {
			t.Errorf("unsupported field %s is exposed in the schema", name)
		}
	}
}

func TestNodePoolRolloutAdmission(t *testing.T) {
	schema := loadResourceSchema(t, "nkeclusters")
	sample := clusterSample(t)
	budgetPath := []string{"rollout", "strategy", "disruption", "budgets"}
	for _, tt := range []fieldCase{
		{name: "omitted rollout", path: []string{"rollout"}, remove: true},
		{name: "missing strategy", path: []string{"rollout", "strategy"}, remove: true, invalid: true},
		{name: "unsupported strategy", path: []string{"rollout", "strategy", "type"}, value: "Recreate", invalid: true},
		{name: "missing disruption", path: []string{"rollout", "strategy", "disruption"}, remove: true, invalid: true},
		{name: "missing budgets", path: budgetPath, remove: true, invalid: true},
		{name: "empty budgets", path: budgetPath, value: []any{}, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			obj := runtime.DeepCopyJSON(sample)
			pool := specOf(t, obj)["nodePools"].([]any)[0].(map[string]any)
			tt.apply(pool)
			schema.check(t, tt.invalid, obj, nil)
		})
	}
	for _, tt := range []struct {
		name    string
		budget  map[string]any
		invalid bool
	}{
		{name: "one node", budget: map[string]any{"nodes": "1"}},
		{name: "multiple nodes", budget: map[string]any{"nodes": "2"}},
		{name: "pause", budget: map[string]any{"nodes": "0"}},
		{name: "percentage", budget: map[string]any{"nodes": "10%"}},
		{name: "full percentage", budget: map[string]any{"nodes": "100%"}},
		{name: "zero percentage", budget: map[string]any{"nodes": "0%"}},
		{name: "missing nodes", budget: map[string]any{}, invalid: true},
		{name: "numeric nodes", budget: map[string]any{"nodes": int64(1)}, invalid: true},
		{name: "negative nodes", budget: map[string]any{"nodes": "-1"}, invalid: true},
		{name: "excess percentage", budget: map[string]any{"nodes": "101%"}, invalid: true},
		{name: "fractional percentage", budget: map[string]any{"nodes": "1.5%"}, invalid: true},
		{name: "oversized count", budget: map[string]any{"nodes": "2147483648"}, invalid: true},
		{name: "scheduled window", budget: map[string]any{"nodes": "0", "schedule": "0 9 * * mon-fri", "duration": "8h"}},
		{name: "combined duration", budget: map[string]any{"nodes": "1", "schedule": "0 9 * * *", "duration": "1h30m"}},
		{name: "schedule without duration", budget: map[string]any{"nodes": "0", "schedule": "0 9 * * *"}, invalid: true},
		{name: "duration without schedule", budget: map[string]any{"nodes": "0", "duration": "8h"}, invalid: true},
		{name: "blank schedule", budget: map[string]any{"nodes": "0", "schedule": " ", "duration": "8h"}, invalid: true},
		{name: "zero duration", budget: map[string]any{"nodes": "0", "schedule": "0 9 * * *", "duration": "0s"}, invalid: true},
		{name: "invalid duration", budget: map[string]any{"nodes": "0", "schedule": "0 9 * * *", "duration": "tomorrow"}, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			obj := runtime.DeepCopyJSON(sample)
			pool := specOf(t, obj)["nodePools"].([]any)[0].(map[string]any)
			setField(pool, budgetPath, []any{tt.budget})
			schema.check(t, tt.invalid, obj, nil)
		})
	}
}

func TestRolloutScheduleFormat(t *testing.T) {
	schema := loadResourceSchema(t, "nkeclusters")
	sample := clusterSample(t)
	for _, tt := range []struct {
		schedule string
		invalid  bool
	}{
		{schedule: "0 9 * * mon-fri"},
		{schedule: "*/15 9-17 * * MON-FRI"},
		{schedule: "0,30 8,17 1-15 JAN,MAR MON,WED,FRI"},
		{schedule: "0 9 ? * MON"},
		{schedule: "  0\t9  * * mon-fri  "},
		{schedule: "@annually"},
		{schedule: "@yearly"},
		{schedule: "@monthly"},
		{schedule: "@weekly"},
		{schedule: "@daily"},
		{schedule: "@midnight"},
		{schedule: "@hourly"},
		{schedule: "", invalid: true},
		{schedule: " ", invalid: true},
		{schedule: "not a cron", invalid: true},
		{schedule: "0 9 * *", invalid: true},
		{schedule: "0 0 9 * * *", invalid: true},
		{schedule: "@daily trailing", invalid: true},
		{schedule: "@reboot", invalid: true},
		{schedule: "@every 1h", invalid: true},
		{schedule: "CRON_TZ=UTC 0 9 * * *", invalid: true},
	} {
		t.Run(tt.schedule, func(t *testing.T) {
			obj := runtime.DeepCopyJSON(sample)
			pool := specOf(t, obj)["nodePools"].([]any)[0].(map[string]any)
			setField(pool, []string{"rollout", "strategy", "disruption", "budgets"}, []any{
				map[string]any{"nodes": "0", "schedule": tt.schedule, "duration": "8h"},
			})
			schema.check(t, tt.invalid, obj, nil)
		})
	}
}

func TestNodePoolIdentity(t *testing.T) {
	schema := loadResourceSchema(t, "nkeclusters")
	old := clusterSample(t)
	pool := specOf(t, old)["nodePools"].([]any)[0].(map[string]any)
	second := runtime.DeepCopyJSON(pool)
	second["name"] = "gpu-pool-2"
	second["instanceType"] = "gpu-small"
	second["vpcSubnetID"] = "020d9255-678c-4e33-9c6a-20c378353dc0"
	specOf(t, old)["nodePools"] = []any{pool, second}
	schema.admit(t, old, nil)

	for _, tt := range []struct {
		name    string
		change  func([]any) []any
		invalid bool
	}{
		{name: "reorder pools", change: func(p []any) []any { return []any{p[1], p[0]} }},
		{name: "scale pool", change: func(p []any) []any { p[0].(map[string]any)["replicas"] = int64(0); return p }},
		{name: "update profile", change: func(p []any) []any { p[0].(map[string]any)["nodeProfile"] = "ubuntu-24.04:v2"; return p }},
		{name: "remove pool", change: func(p []any) []any { return p[1:] }},
		{name: "change instance type", change: func(p []any) []any { p[0].(map[string]any)["instanceType"] = "other"; return p }, invalid: true},
		{name: "duplicate pool name", change: func(p []any) []any { p[1].(map[string]any)["name"] = p[0].(map[string]any)["name"]; return p }, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			obj := runtime.DeepCopyJSON(old)
			spec := specOf(t, obj)
			spec["nodePools"] = tt.change(spec["nodePools"].([]any))
			schema.check(t, tt.invalid, obj, runtime.DeepCopyJSON(old))
		})
	}
}

func TestNodePoolDeepCopy(t *testing.T) {
	original := decodeJSON[NKECluster](t, encodeJSON(t, clusterSample(t)))
	original.Spec.NodePools[0].UserParams.Taints = []NodeTaint{{Key: "workload", Value: "gpu", Effect: "NoSchedule"}}
	zero := int32(0)
	original.Status.NodePools = []NodePoolStatus{{Name: "gpu-pool", Replicas: &zero}}
	clone := original.DeepCopy()
	*clone.Spec.NodePools[0].Replicas = 0
	clone.Spec.NodePools[0].UserParams.Labels["workload"] = "inference"
	clone.Spec.NodePools[0].UserParams.Taints[0].Value = "other"
	clone.Spec.NodePools[0].Rollout.Strategy.Disruption.Budgets[0].Nodes = "0"
	*clone.Status.NodePools[0].Replicas = 1
	if *original.Spec.NodePools[0].Replicas != 8 || original.Spec.NodePools[0].UserParams.Labels["workload"] != "pretraining" ||
		original.Spec.NodePools[0].UserParams.Taints[0].Value != "gpu" || *original.Status.NodePools[0].Replicas != 0 {
		t.Fatal("deepcopy mutation changed the cached cluster")
	}
	if original.Spec.NodePools[0].Rollout.Strategy.Disruption.Budgets[0].Nodes != "10%" {
		t.Fatal("deepcopy mutation changed the original rollout budget")
	}
	obj := decodeJSON[map[string]any](t, encodeJSON(t, original))
	loadResourceSchema(t, "nkeclusters").admit(t, obj, nil)
}

func TestNodePoolUserParams(t *testing.T) {
	schema := loadResourceSchema(t, "nkeclusters")
	labels := []string{"userParams", "labels"}
	taints := []string{"userParams", "taints"}
	for _, c := range []fieldCase{
		{name: "tenant labels", path: labels, value: map[string]any{"example.com/team": "research", "workload": ""}},
		{name: "reserved NKE label", path: labels, value: map[string]any{"nke.nvidia.com/role": "worker"}, invalid: true},
		{name: "reserved Kubernetes label", path: labels, value: map[string]any{"kubernetes.io/role": "worker"}, invalid: true},
		{name: "reserved Kubernetes subdomain", path: labels, value: map[string]any{"topology.kubernetes.io/foo": "bar"}, invalid: true},
		{name: "reserved k8s label", path: labels, value: map[string]any{"k8s.io/foo": "bar"}, invalid: true},
		{name: "invalid label key", path: labels, value: map[string]any{"bad key": "worker"}, invalid: true},
		{name: "invalid label prefix", path: labels, value: map[string]any{"Example.com/team": "worker"}, invalid: true},
		{name: "invalid label value", path: labels, value: map[string]any{"team": "bad value"}, invalid: true},
		{name: "valid taint", path: taints, value: []any{map[string]any{"key": "example.com/workload", "value": "gpu", "effect": "NoSchedule"}}},
		{name: "invalid taint effect", path: taints, value: []any{map[string]any{"key": "workload", "effect": "Unknown"}}, invalid: true},
		{name: "invalid taint key", path: taints, value: []any{map[string]any{"key": "bad key", "effect": "NoExecute"}}, invalid: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			obj := clusterSample(t)
			pool := specOf(t, obj)["nodePools"].([]any)[0].(map[string]any)
			c.apply(pool)
			schema.check(t, c.invalid, obj, nil)
		})
	}
}
