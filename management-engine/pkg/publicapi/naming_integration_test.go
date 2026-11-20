//go:build integration_test

// SPDX-License-Identifier: Apache-2.0

package publicapi

import (
	"context"
	"net/http"
	"reflect"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"
)

// fixtureWireValue selects names from declared fields when reusing canonical source fixtures.
// Dictionary keys and scalar values belong to the caller and remain unchanged.
func fixtureWireValue(goType reflect.Type, value any, naming resource.Naming) any {
	for goType.Kind() == reflect.Pointer {
		goType = goType.Elem()
	}
	switch value := value.(type) {
	case map[string]any:
		result := map[string]any{}
		if goType.Kind() == reflect.Map {
			for key, child := range value {
				result[key] = fixtureWireValue(goType.Elem(), child, naming)
			}
			return result
		}
		gomega.Expect(goType.Kind()).To(gomega.Equal(reflect.Struct))
		for field := range goType.Fields() {
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if child, present := value[name]; present && field.IsExported() && name != "" && name != "-" {
				result[naming.FieldName(name)] = fixtureWireValue(field.Type, child, naming)
			}
		}
		gomega.Expect(result).To(gomega.HaveLen(len(value)), "fixture contains undeclared fields of %s", goType)
		return result
	case []any:
		result := make([]any, len(value))
		for i, child := range value {
			result[i] = fixtureWireValue(goType.Elem(), child, naming)
		}
		return result
	default:
		return value
	}
}

func (api *apiConformance) exerciseNaming(ctx context.Context, datastore client.Client, canonical map[string]any) {
	ginkgo.GinkgoHelper()
	ginkgo.By("snake-case fields, acronym names, and unchanged map keys")
	canonical = runtime.DeepCopyJSON(canonical)
	pools := canonical["nodePools"].([]any)
	pool := pools[0].(map[string]any)
	labels := map[string]any{"example.com/teamName": "CamelCaseValue", "mixedCase": "UPPER_value"}
	pool["userParams"] = map[string]any{"labels": labels}
	pool["replicas"] = int64(0)
	wire := fixtureWireValue(reflect.TypeFor[v1.NVKEClusterSpec](), canonical, api.naming).(map[string]any)
	_, created := api.checkExchange(ctx, 201, http.MethodPost, "/v1/clusters", nil,
		resourceBody("naming-boundary", wire), true)
	spec := created["spec"].(map[string]any)
	gomega.Expect(spec).To(gomega.HaveKey("api_server_access"))
	gomega.Expect(spec).NotTo(gomega.HaveKey("apiServerAccess"))
	gomega.Expect(spec["api_server_access"]).To(gomega.HaveKeyWithValue("public_access_enabled", false))
	gomega.Expect(spec["network"]).To(gomega.HaveKeyWithValue("vpc_id", canonical["network"].(map[string]any)["vpcID"]))
	wirePool := spec["node_pools"].([]any)[0].(map[string]any)
	gomega.Expect(wirePool).To(gomega.HaveKeyWithValue("vpc_subnet_id", pool["vpcSubnetID"]))
	gomega.Expect(wirePool["replicas"]).To(gomega.Equal(float64(0)))
	gomega.Expect(wirePool["user_params"]).To(gomega.HaveKeyWithValue("labels", labels))
	gomega.Expect(wirePool["rollout"].(map[string]any)["strategy"]).To(gomega.HaveKeyWithValue("type", "RollingUpdate"))
	gomega.Expect(created["metadata"]).To(gomega.HaveKey("creation_timestamp"))

	ginkgo.By("canonical datastore keys behind the public transport")
	stored := &unstructured.Unstructured{}
	stored.SetGroupVersionKind(v1.SchemeGroupVersion.WithKind("NVKECluster"))
	gomega.Expect(datastore.Get(ctx, client.ObjectKey{Namespace: "org-a", Name: "naming-boundary"}, stored)).To(gomega.Succeed())
	storedSpec := stored.Object["spec"].(map[string]any)
	gomega.Expect(storedSpec).To(gomega.HaveKey("apiServerAccess"))
	gomega.Expect(storedSpec).NotTo(gomega.HaveKey("api_server_access"))
	gomega.Expect(storedSpec["network"]).To(gomega.HaveKeyWithValue("vpcID", canonical["network"].(map[string]any)["vpcID"]))
	storedPool := storedSpec["nodePools"].([]any)[0].(map[string]any)
	gomega.Expect(storedPool["userParams"]).To(gomega.HaveKeyWithValue("labels", labels))
	stored.Object["status"] = map[string]any{
		"observedGeneration": int64(1),
		"conditions": []any{map[string]any{
			"type": "Ready", "status": "False", "observedGeneration": int64(1),
			"lastTransitionTime": "2026-01-01T00:00:00Z", "reason": "WaitingForNodes", "message": "Waiting for nodes.",
		}},
	}
	gomega.Expect(datastore.Status().Update(ctx, stored)).To(gomega.Succeed())
	_, current := api.checkExchange(ctx, 200, http.MethodGet, "/v1/clusters/naming-boundary", nil, nil, true)
	status := current["status"].(map[string]any)
	gomega.Expect(status).To(gomega.HaveKeyWithValue("observed_generation", float64(1)))
	condition := status["conditions"].([]any)[0].(map[string]any)
	gomega.Expect(condition).To(gomega.HaveKey("last_transition_time"))
	gomega.Expect(condition).To(gomega.HaveKeyWithValue("reason", "WaitingForNodes"))

	ginkgo.By("rejecting alternate spellings of declared fields")
	for _, alternate := range []string{"apiServerAccess", "APIServerAccess", "API_SERVER_ACCESS"} {
		invalid := runtime.DeepCopyJSON(wire)
		invalid[alternate] = invalid["api_server_access"]
		delete(invalid, "api_server_access")
		_, problem := api.checkExchange(ctx, 400, http.MethodPost, "/v1/clusters", nil,
			resourceBody("alternate-name", invalid), false)
		gomega.Expect(problem["code"]).To(gomega.Equal("invalid_request"))
	}
	both := runtime.DeepCopyJSON(wire)
	both["apiServerAccess"] = both["api_server_access"]
	api.checkExchange(ctx, 400, http.MethodPost, "/v1/clusters", nil, resourceBody("both-names", both), false)
	for _, alternate := range []string{"vpcID", "vpcId"} {
		invalid := runtime.DeepCopyJSON(wire)
		network := invalid["network"].(map[string]any)
		network[alternate] = network["vpc_id"]
		delete(network, "vpc_id")
		api.checkExchange(ctx, 400, http.MethodPost, "/v1/clusters", nil, resourceBody("alternate-id", invalid), false)
	}

	ginkgo.By("snake-case validation pointers and error codes")
	invalid := runtime.DeepCopyJSON(wire)
	invalid["node_pools"].([]any)[0].(map[string]any)["replicas"] = int64(-1)
	_, problem := api.checkExchange(ctx, 422, http.MethodPost, "/v1/clusters", nil, resourceBody("invalid-replicas", invalid), false)
	gomega.Expect(problem["code"]).To(gomega.Equal("invalid_resource"))
	gomega.Expect(problem["errors"]).To(gomega.ContainElement(gomega.HaveKeyWithValue("field", "/spec/node_pools/0/replicas")))
	for _, query := range []string{"pageSize=1", "pageKey="} {
		_, problem := api.checkExchange(ctx, 400, http.MethodGet, "/v1/clusters?"+query, nil, nil, true)
		gomega.Expect(problem["code"]).To(gomega.Equal("invalid_request"))
		gomega.Expect(problem["errors"]).To(gomega.ContainElement(gomega.HaveKeyWithValue("code", "invalid_query")))
	}
}
