// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"os"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	schemacel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/listtype"
	apivalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// These tests run Kubernetes' schema and CEL validators against the generated
// CRDs. They do not simulate authorization, admission webhooks or the status
// subresource.

// resourceSchema is one generated CRD, ready to admit candidate resources.
type resourceSchema struct {
	structural *structuralschema.Structural
	openAPI    apivalidation.SchemaValidator
	cel        *schemacel.Validator
}

func loadResourceSchema(t *testing.T, resource string) resourceSchema {
	t.Helper()
	data, err := os.ReadFile("../crds/internal.nke.nvidia.com_" + resource + ".yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(data, &crd); err != nil {
		t.Fatal(err)
	}
	apiextensionsv1.SetDefaults_CustomResourceDefinition(&crd)

	var internal apiextensions.CustomResourceDefinition
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&crd, &internal, nil); err != nil {
		t.Fatal(err)
	}
	// This also checks structural rules, default validity and CEL compile/cost limits.
	if errs := crdvalidation.ValidateCustomResourceDefinition(context.Background(), &internal); len(errs) != 0 {
		t.Fatalf("invalid generated CRD: %v", errs)
	}

	schema := storageVersionSchema(t, internal)
	structural, err := structuralschema.NewStructural(schema)
	if err != nil {
		t.Fatal(err)
	}
	openAPI, _, err := apivalidation.NewSchemaValidator(schema)
	if err != nil {
		t.Fatal(err)
	}
	return resourceSchema{
		structural: structural,
		openAPI:    openAPI,
		// Nil when the schema carries no CEL rules, which validate allows for.
		cel: schemacel.NewValidator(structural, true, celconfig.PerCallLimit),
	}
}

func storageVersionSchema(t *testing.T, crd apiextensions.CustomResourceDefinition) *apiextensions.JSONSchemaProps {
	t.Helper()
	if len(crd.Spec.Versions) != 1 || !crd.Spec.Versions[0].Served || !crd.Spec.Versions[0].Storage {
		t.Fatal("expected one served storage version")
	}
	validation := crd.Spec.Versions[0].Schema
	if validation == nil {
		// Conversion lifts a schema shared by all versions to spec.validation.
		validation = crd.Spec.Validation
	}
	if validation == nil || validation.OpenAPIV3Schema == nil {
		t.Fatal("generated CRD has no validation schema")
	}
	return validation.OpenAPIV3Schema
}

// validate admits obj, as an update to old when old is non-nil.
func (s resourceSchema) validate(obj, old map[string]any) field.ErrorList {
	structuraldefaulting.Default(obj, s.structural)
	var errs field.ErrorList
	var previous any
	if old == nil {
		errs = apivalidation.ValidateCustomResource(nil, obj, s.openAPI)
	} else {
		structuraldefaulting.Default(old, s.structural)
		previous = old
		errs = apivalidation.ValidateCustomResourceUpdate(nil, obj, old, s.openAPI)
	}
	if s.cel != nil {
		celErrs, _ := s.cel.Validate(context.Background(), nil, s.structural, obj, previous, celconfig.RuntimeCELCostBudget)
		errs = append(errs, celErrs...)
	}
	return append(errs, listtype.ValidateListSetsAndMaps(nil, s.structural, obj)...)
}

// check requires obj to be rejected exactly when invalid is true.
func (s resourceSchema) check(t *testing.T, invalid bool, obj, old map[string]any) {
	t.Helper()
	if errs := s.validate(obj, old); (len(errs) != 0) != invalid {
		t.Fatalf("invalid=%v, validation errors: %v", invalid, errs)
	}
}

// admit requires obj to pass, reading better than check with a false literal.
func (s resourceSchema) admit(t *testing.T, obj, old map[string]any) {
	t.Helper()
	s.check(t, false, obj, old)
}

// fieldCase replaces or removes one field of a resource and says whether
// admission should then fail.
type fieldCase struct {
	name    string
	path    []string
	value   any
	remove  bool
	invalid bool
}

func (c fieldCase) apply(obj map[string]any) {
	if c.remove {
		deleteField(obj, c.path)
		return
	}
	setField(obj, c.path, c.value)
}

// runCreate applies each case to its own copy of the named base resource.
func (s resourceSchema) runCreate(t *testing.T, resource string, cases []fieldCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj := baseResource(t, resource)
			c.apply(obj)
			s.check(t, c.invalid, obj, nil)
		})
	}
}

// runUpdate applies each case to a copy of old and admits it as an update.
func (s resourceSchema) runUpdate(t *testing.T, old map[string]any, cases []fieldCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj := runtime.DeepCopyJSON(old)
			c.apply(obj)
			s.check(t, c.invalid, obj, runtime.DeepCopyJSON(old))
		})
	}
}

// Minimal resources that pass admission: every required field and nothing else.
var baseResources = map[string]string{
	"nkeclusters": `{
		"apiVersion": "internal.nke.nvidia.com/v1alpha1",
		"kind": "NKECluster",
		"metadata": {"name": "demo", "namespace": "org-demo"},
		"spec": {
			"apiServerAccess": {"publicAccessEnabled": false},
			"clusterNetwork": {
				"pods": {"cidrBlocks": ["192.168.0.0/16"]},
				"services": {"cidrBlocks": ["10.96.0.0/12"]},
				"serviceDomain": "cluster.local"
			},
			"release": {"channel": {"name": "stable"}},
			"controlPlaneClass": "standard"
		}
	}`,
	"nodepools": `{
		"apiVersion": "internal.nke.nvidia.com/v1alpha1",
		"kind": "NodePool",
		"metadata": {"name": "workers", "namespace": "org-demo"},
		"spec": {
			"clusterName": "demo",
			"replicas": 2,
			"instanceType": "gpu-small",
			"release": {"name": "ubuntu", "version": "1.0"}
		}
	}`,
}

func baseResource(t *testing.T, resource string) map[string]any {
	t.Helper()
	data, ok := baseResources[resource]
	if !ok {
		t.Fatalf("no base resource for %q", resource)
	}
	return decodeJSON[map[string]any](t, []byte(data))
}

func decodeJSON[T any](t *testing.T, data []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func encodeJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// parentOf walks to the object holding the last path element, creating the
// intermediate objects a test needs to set a field the base resource omits.
func parentOf(obj map[string]any, path []string) (map[string]any, string) {
	for _, key := range path[:len(path)-1] {
		nested, ok := obj[key].(map[string]any)
		if !ok {
			nested = map[string]any{}
			obj[key] = nested
		}
		obj = nested
	}
	return obj, path[len(path)-1]
}

func setField(obj map[string]any, path []string, value any) {
	parent, key := parentOf(obj, path)
	parent[key] = value
}

func deleteField(obj map[string]any, path []string) {
	parent, key := parentOf(obj, path)
	delete(parent, key)
}

func TestGeneratedResourceSchemas(t *testing.T) {
	for _, resource := range []string{"nkeclusters", "nodepools"} {
		t.Run(resource, func(t *testing.T) {
			loadResourceSchema(t, resource).admit(t, baseResource(t, resource), nil)
		})
	}
}

func TestNodePoolSchemaAdmission(t *testing.T) {
	schema := loadResourceSchema(t, "nodepools")
	replicas := []string{"spec", "replicas"}
	labels := []string{"spec", "template", "metadata", "labels"}
	taints := []string{"spec", "template", "spec", "taints"}

	schema.runCreate(t, "nodepools", []fieldCase{
		{name: "zero replicas", path: replicas, value: int64(0)},
		{name: "missing replicas", path: replicas, remove: true, invalid: true},
		{name: "null replicas", path: replicas, invalid: true},
		{name: "negative replicas", path: replicas, value: int64(-1), invalid: true},
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
	})

	t.Run("rollout default", func(t *testing.T) {
		obj := baseResource(t, "nodepools")
		schema.admit(t, obj, nil)
		rollout, ok := obj["spec"].(map[string]any)["rollout"].(map[string]any)
		if !ok {
			t.Fatal("rollout default missing")
		}
		strategy, _ := rollout["strategy"].(map[string]any)
		rollingUpdate, _ := strategy["rollingUpdate"].(map[string]any)
		if strategy["type"] != "RollingUpdate" || rollingUpdate["maxSurge"] != int64(1) {
			t.Fatalf("unexpected rollout default: %#v", rollout)
		}
	})
}

func TestNodePoolImmutableBinding(t *testing.T) {
	schema := loadResourceSchema(t, "nodepools")
	bound := baseResource(t, "nodepools")
	bound["status"] = map[string]any{"clusterRef": map[string]any{"name": "demo", "uid": "cluster-uid"}}
	schema.admit(t, bound, nil)

	schema.runUpdate(t, bound, []fieldCase{
		{name: "scale", path: []string{"spec", "replicas"}, value: int64(5)},
		{name: "change cluster", path: []string{"spec", "clusterName"}, value: "other", invalid: true},
		{name: "change instance type", path: []string{"spec", "instanceType"}, value: "gpu-large", invalid: true},
		{name: "change cluster UID", path: []string{"status", "clusterRef", "uid"}, value: "replacement-uid", invalid: true},
		{name: "remove binding", path: []string{"status", "clusterRef"}, remove: true, invalid: true},
		{name: "remove status", path: []string{"status"}, remove: true, invalid: true},
	})

	// First binding is permitted, but must identify the cluster requested in spec.
	for _, name := range []string{"demo", "different"} {
		t.Run("first binding to "+name, func(t *testing.T) {
			obj := baseResource(t, "nodepools")
			obj["status"] = map[string]any{"clusterRef": map[string]any{"name": name, "uid": "first-uid"}}
			schema.check(t, name != "demo", obj, baseResource(t, "nodepools"))
		})
	}
}

func TestNodePoolRolloutAdmission(t *testing.T) {
	schema := loadResourceSchema(t, "nodepools")
	for _, tt := range []struct {
		name    string
		surge   any
		invalid bool
	}{
		{name: "zero surge", surge: int64(0)},
		{name: "one surge", surge: int64(1)},
		{name: "percentage surge", surge: "25%"},
		{name: "zero percent surge", surge: "0%"},
		{name: "over one hundred percent surge", surge: "200%"},
		{name: "maximum count", surge: int64(2147483647)},
		{name: "maximum percentage numerator", surge: "2147483647%"},
		{name: "count overflow", surge: int64(2147483648), invalid: true},
		{name: "percentage numerator overflow", surge: "2147483648%", invalid: true},
		{name: "unparseable percentage", surge: "999999999999999999999999999999999999999999%", invalid: true},
		{name: "negative surge", surge: int64(-1), invalid: true},
		{name: "numeric string surge", surge: "1", invalid: true},
		{name: "negative percentage surge", surge: "-1%", invalid: true},
		{name: "empty surge", surge: "", invalid: true},
		{name: "missing numerator", surge: "%", invalid: true},
		{name: "leading zero percentage", surge: "01%", invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			obj := baseResource(t, "nodepools")
			setField(obj, []string{"spec", "rollout", "strategy"}, map[string]any{
				"type":          "RollingUpdate",
				"rollingUpdate": map[string]any{"maxSurge": tt.surge},
			})
			schema.check(t, tt.invalid, obj, nil)
			if tt.invalid {
				return
			}
			// Whatever the schema admits must also decode and scale in Go.
			pool := decodeJSON[NodePool](t, encodeJSON(t, obj))
			if _, err := intstr.GetScaledValueFromIntOrPercent(&pool.Spec.Rollout.Strategy.RollingUpdate.MaxSurge, 2, true); err != nil {
				t.Fatalf("admitted surge cannot be scaled: %v", err)
			}
		})
	}

	t.Run("empty rollout", func(t *testing.T) {
		obj := baseResource(t, "nodepools")
		setField(obj, []string{"spec", "rollout"}, map[string]any{})
		schema.check(t, true, obj, nil)
	})
}

func TestNKEClusterSchemaAdmission(t *testing.T) {
	channelVersion := []string{"spec", "release", "channel", "version"}
	acceleratedRuntime := []string{"spec", "acceleratedRuntime"}

	loadResourceSchema(t, "nkeclusters").runCreate(t, "nkeclusters", []fieldCase{
		{name: "channel with preferred version", path: channelVersion, value: "1.35"},
		{name: "missing channel", path: []string{"spec", "release", "channel"}, remove: true, invalid: true},
		{name: "empty channel name", path: []string{"spec", "release", "channel", "name"}, value: "", invalid: true},
		{name: "empty channel version", path: channelVersion, value: "", invalid: true},
		{name: "runtime opt out", path: acceleratedRuntime, value: map[string]any{"name": "none"}},
		{name: "runtime release", path: acceleratedRuntime, value: map[string]any{"name": "gpu", "version": "1.0"}},
		{name: "opt out with version", path: acceleratedRuntime, value: map[string]any{"name": "none", "version": "1.0"}, invalid: true},
		{name: "runtime missing version", path: acceleratedRuntime, value: map[string]any{"name": "gpu"}, invalid: true},
		{name: "invalid network CIDR", path: []string{"spec", "clusterNetwork", "pods", "cidrBlocks"}, value: []any{"invalid"}, invalid: true},
	})
}

// NetworkRanges is shared by pods and services, so the dual-stack rule is
// checked on both rather than on services alone.
func TestNKEClusterNetworkFamilies(t *testing.T) {
	schema := loadResourceSchema(t, "nkeclusters")
	for _, network := range []string{"pods", "services"} {
		t.Run(network, func(t *testing.T) {
			path := []string{"spec", "clusterNetwork", network, "cidrBlocks"}
			for _, tt := range []struct {
				name    string
				items   string
				invalid bool
			}{
				{name: "single IPv4", items: `["10.96.0.0/12"]`},
				{name: "single IPv6", items: `["fd00:1234::/108"]`},
				{name: "IPv4 primary", items: `["10.96.0.0/12","fd00:1234::/108"]`},
				{name: "IPv6 primary", items: `["fd00:1234::/108","10.96.0.0/12"]`},
				{name: "two IPv4 CIDRs", items: `["10.96.0.0/12","10.97.0.0/16"]`, invalid: true},
				{name: "two IPv6 CIDRs", items: `["fd00:1::/108","fd00:2::/108"]`, invalid: true},
			} {
				t.Run(tt.name, func(t *testing.T) {
					obj := baseResource(t, "nkeclusters")
					setField(obj, path, decodeJSON[[]any](t, []byte(tt.items)))
					schema.check(t, tt.invalid, obj, nil)
				})
			}
		})
	}
}

const testIssuerURL = "https://issuer.example.com"

// jwtAuthenticator is the smallest authenticator the schema admits.
func jwtAuthenticator(issuer string) map[string]any {
	return map[string]any{
		"issuer":        map[string]any{"url": issuer, "audiences": []any{"nke"}},
		"claimMappings": map[string]any{"username": map[string]any{"claim": "sub", "prefix": "tenant:"}},
	}
}

func setAuthenticators(obj map[string]any, jwt ...any) {
	setField(obj, []string{"spec", "authentication"}, map[string]any{"jwt": jwt})
}

// clusterWithIssuer returns a base cluster trusting issuer, or trusting nobody
// when issuer is empty.
func clusterWithIssuer(t *testing.T, issuer string) map[string]any {
	t.Helper()
	obj := baseResource(t, "nkeclusters")
	if issuer != "" {
		setAuthenticators(obj, jwtAuthenticator(issuer))
	}
	return obj
}

// TestNKEClusterJWTAdmission mutates the authenticator rather than the cluster,
// so it drives fieldCase over the nested object instead of using runCreate.
func TestNKEClusterJWTAdmission(t *testing.T) {
	schema := loadResourceSchema(t, "nkeclusters")
	issuerURL := []string{"issuer", "url"}
	audiences := []string{"issuer", "audiences"}
	usernamePrefix := []string{"claimMappings", "username", "prefix"}

	for _, c := range []fieldCase{
		{name: "explicit empty username prefix", path: usernamePrefix, value: ""},
		{name: "missing issuer URL", path: issuerURL, remove: true, invalid: true},
		{name: "HTTP issuer", path: issuerURL, value: "http://issuer.example.com", invalid: true},
		{name: "empty audiences", path: audiences, value: []any{}, invalid: true},
		{name: "empty audience", path: audiences, value: []any{""}, invalid: true},
		{name: "missing username mapping", path: []string{"claimMappings", "username"}, remove: true, invalid: true},
		{name: "missing username prefix", path: usernamePrefix, remove: true},
		{name: "empty username claim", path: []string{"claimMappings", "username", "claim"}, value: "", invalid: true},
		{name: "explicit empty validation rules", path: []string{"claimValidationRules"}, value: []any{}, invalid: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			jwt := jwtAuthenticator(testIssuerURL)
			c.apply(jwt)
			obj := baseResource(t, "nkeclusters")
			setAuthenticators(obj, jwt)
			schema.check(t, c.invalid, obj, nil)
		})
	}
}

func TestNKEClusterJWTProviderLimits(t *testing.T) {
	schema := loadResourceSchema(t, "nkeclusters")
	for _, tt := range []struct {
		name      string
		count     int
		duplicate bool
		invalid   bool
	}{
		{name: "explicit empty issuer list", count: 0, invalid: true},
		{name: "distinct issuers", count: 2},
		{name: "duplicate issuer", count: 2, duplicate: true, invalid: true},
		{name: "sixty issuers", count: 60},
		{name: "sixty one issuers", count: 61, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			providers := make([]any, tt.count)
			for i := range providers {
				issuer := fmt.Sprintf("https://issuer-%d.example.com", i)
				if tt.duplicate {
					issuer = testIssuerURL
				}
				providers[i] = jwtAuthenticator(issuer)
			}
			obj := baseResource(t, "nkeclusters")
			setAuthenticators(obj, providers...)
			schema.check(t, tt.invalid, obj, nil)
		})
	}
}

func TestNKEClusterAuthenticationCreateOnly(t *testing.T) {
	schema := loadResourceSchema(t, "nkeclusters")
	const replacementURL = "https://replacement.example.com"

	for _, tt := range []struct {
		name      string
		oldIssuer string
		newIssuer string
		invalid   bool
	}{
		{name: "unchanged absent"},
		{name: "unchanged configured", oldIssuer: testIssuerURL, newIssuer: testIssuerURL},
		{name: "add after creation", newIssuer: testIssuerURL, invalid: true},
		{name: "remove after creation", oldIssuer: testIssuerURL, invalid: true},
		{name: "change after creation", oldIssuer: testIssuerURL, newIssuer: replacementURL, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			old := clusterWithIssuer(t, tt.oldIssuer)
			schema.admit(t, old, nil)
			schema.check(t, tt.invalid, clusterWithIssuer(t, tt.newIssuer), old)
		})
	}
}

func TestResourceCollectionUniqueness(t *testing.T) {
	schemas := map[string]resourceSchema{
		"nkeclusters": loadResourceSchema(t, "nkeclusters"),
		"nodepools":   loadResourceSchema(t, "nodepools"),
	}
	taints := []string{"spec", "template", "spec", "taints"}
	serviceCIDRs := []string{"spec", "clusterNetwork", "services", "cidrBlocks"}

	for _, tt := range []struct {
		name     string
		resource string
		path     []string
		items    string
		invalid  bool
	}{
		{
			name: "duplicate taint key and effect", resource: "nodepools", path: taints,
			items:   `[{"key":"workload","effect":"NoSchedule","value":"a"},{"key":"workload","effect":"NoSchedule","value":"b"}]`,
			invalid: true,
		},
		{
			name: "same taint key with distinct effects", resource: "nodepools", path: taints,
			items: `[{"key":"workload","effect":"NoSchedule"},{"key":"workload","effect":"NoExecute"}]`,
		},
		{
			name: "duplicate condition type", resource: "nodepools", path: []string{"status", "conditions"},
			items: `[{"type":"Ready","status":"True","reason":"Ready","message":"","lastTransitionTime":"2026-01-01T00:00:00Z"},
			         {"type":"Ready","status":"False","reason":"Pending","message":"","lastTransitionTime":"2026-01-01T00:00:00Z"}]`,
			invalid: true,
		},
		{
			name: "duplicate access CIDR", resource: "nkeclusters", path: []string{"spec", "apiServerAccess", "publicAccessCidrs"},
			items:   `[{"cidr":"10.0.0.0/8"},{"cidr":"10.0.0.0/8","description":"duplicate"}]`,
			invalid: true,
		},
		{
			name: "duplicate audience", resource: "nkeclusters", path: []string{"spec", "authentication", "jwt"},
			items:   `[{"issuer":{"url":"https://issuer.example.com","audiences":["nke","nke"]},"claimMappings":{"username":{"claim":"sub"}}}]`,
			invalid: true,
		},
		{
			name: "duplicate network CIDR", resource: "nkeclusters", path: serviceCIDRs,
			items:   `["10.96.0.0/12","10.96.0.0/12"]`,
			invalid: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			obj := baseResource(t, tt.resource)
			setField(obj, tt.path, decodeJSON[[]any](t, []byte(tt.items)))
			schemas[tt.resource].check(t, tt.invalid, obj, nil)
		})
	}
}
