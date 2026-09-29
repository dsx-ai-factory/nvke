// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"fmt"
	"os"
	"strings"
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

// loadCRD reads a generated CRD as controller-gen emitted it, before defaulting
// and conversion, so tests can read the markers back off the schema.
func loadCRD(t *testing.T, resource string) apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	data, err := os.ReadFile("../crds/internal.nke.nvidia.com_" + resource + ".yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(data, &crd); err != nil {
		t.Fatal(err)
	}
	return crd
}

func loadResourceSchema(t *testing.T, resource string) resourceSchema {
	t.Helper()
	crd := loadCRD(t, resource)
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
	if len(crd.Spec.Versions) != 1 || !crd.Spec.Versions[0].Served || !crd.Spec.Versions[0].Storage || crd.Spec.Versions[0].Name != GroupVersion.Version {
		t.Fatalf("expected one served storage version: %s", GroupVersion.Version)
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
		"apiVersion": "internal.nke.nvidia.com/v1",
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
		"apiVersion": "internal.nke.nvidia.com/v1",
		"kind": "NodePool",
		"metadata": {"name": "workers", "namespace": "org-demo"},
		"spec": {
			"clusterName": "demo",
			"replicas": 2,
			"instanceType": "gpu-small",
			"release": {"name": "ubuntu", "version": "1.0"}
		}
	}`,
	"machineimages": `{
		"apiVersion": "internal.nke.nvidia.com/v1",
		"kind": "MachineImage",
		"metadata": {"name": "nke-nico-vms-24.04-amd64-fde"},
		"spec": {
			"operatingSystem": {"family": "ubuntu", "version": "24.04"},
			"architecture": "amd64",
			"variant": "fde",
			"buildDate": "2026-03-23",
			"nico": {"ipxeScript": "#!ipxe\nchain ${boot-loader} disk_strategy={{ .DiskSelectionStrategy }}\n"}
		}
	}`,
	"clusterreleases": `{
		"apiVersion": "internal.nke.nvidia.com/v1",
		"kind": "ClusterRelease",
		"metadata": {"name": "nke-nico-vms-24.04-amd64-fde"},
		"spec": {
			"machineImageRef": {"name": "nke-nico-vms-24.04-amd64-fde"},
			"machineInit": {"userDataTemplate": "#cloud-config\nhostname: {{ .Hostname }}\n"}
		}
	}`,
	"nodepoolreleases": `{
		"apiVersion": "internal.nke.nvidia.com/v1",
		"kind": "NodePoolRelease",
		"metadata": {"name": "nke-nico-vms-24.04-amd64-fde"},
		"spec": {
			"machineImageRef": {"name": "nke-nico-vms-24.04-amd64-fde"},
			"machineInit": {"userDataTemplate": "#cloud-config\nhostname: {{ .Hostname }}\n"}
		}
	}`,
}

// releaseResources are the two kinds that share the release shape. Every rule
// below is asserted on both, so neither drifts from the other unnoticed.
var releaseResources = []string{"clusterreleases", "nodepoolreleases"}

func baseResource(t *testing.T, resource string) map[string]any {
	t.Helper()
	data, ok := baseResources[resource]
	if !ok {
		t.Fatalf("no base resource for %q", resource)
	}
	return decodeJSON[map[string]any](t, []byte(data))
}

func specOf(t *testing.T, obj map[string]any) map[string]any {
	t.Helper()
	spec, ok := obj["spec"].(map[string]any)
	if !ok {
		t.Fatalf("resource has no spec: %#v", obj)
	}
	return spec
}

func ipxe(script string) map[string]any {
	return map[string]any{"ipxeScript": script}
}

func userData(template string) map[string]any {
	return map[string]any{"userDataTemplate": template}
}

// mutate copies the base resource and applies change to its spec.
func mutate(t *testing.T, resource string, change func(spec map[string]any)) map[string]any {
	t.Helper()
	obj := baseResource(t, resource)
	change(specOf(t, obj))
	return obj
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

func TestMachineImageAdmission(t *testing.T) {
	schema := loadResourceSchema(t, "machineimages")
	for _, tt := range []struct {
		name    string
		change  func(spec map[string]any)
		invalid bool
	}{
		{name: "arm64", change: func(s map[string]any) { s["architecture"] = "arm64" }},
		{name: "no variant", change: func(s map[string]any) { delete(s, "variant") }},
		// Neither vocabulary is enumerated, so an architecture or distribution
		// nobody has qualified yet is still a well-formed resource.
		{name: "unqualified architecture", change: func(s map[string]any) { s["architecture"] = "riscv64" }},
		{name: "unqualified OS family", change: func(s map[string]any) { s["operatingSystem"] = map[string]any{"family": "debian", "version": "12"} }},
		{name: "uppercase architecture", change: func(s map[string]any) { s["architecture"] = "AMD64" }, invalid: true},
		{name: "empty architecture", change: func(s map[string]any) { s["architecture"] = "" }, invalid: true},
		{name: "missing architecture", change: func(s map[string]any) { delete(s, "architecture") }, invalid: true},
		{name: "uppercase OS family", change: func(s map[string]any) { s["operatingSystem"] = map[string]any{"family": "Ubuntu", "version": "24.04"} }, invalid: true},
		{name: "missing OS version", change: func(s map[string]any) { s["operatingSystem"] = map[string]any{"family": "ubuntu"} }, invalid: true},
		{name: "non-numeric OS version", change: func(s map[string]any) { s["operatingSystem"] = map[string]any{"family": "ubuntu", "version": "noble"} }, invalid: true},
		{name: "uppercase variant", change: func(s map[string]any) { s["variant"] = "FDE" }, invalid: true},
		{name: "build date with time", change: func(s map[string]any) { s["buildDate"] = "2026-03-23T00:00:00Z" }, invalid: true},
		{name: "build date not a date", change: func(s map[string]any) { s["buildDate"] = "March 2026" }, invalid: true},
		{name: "missing boot contract", change: func(s map[string]any) { delete(s, "nico") }, invalid: true},
		{name: "empty iPXE script", change: func(s map[string]any) { s["nico"] = ipxe("") }, invalid: true},
		{name: "whitespace iPXE script", change: func(s map[string]any) { s["nico"] = ipxe("   \n  ") }, invalid: true},
		{name: "iPXE script without the magic line", change: func(s map[string]any) { s["nico"] = ipxe("chain ${boot-loader}\n") }, invalid: true},
		// iPXE itself requires the magic on the first line, so a blank line
		// ahead of it is a broken script rather than harmless formatting.
		{name: "blank line before the magic line", change: func(s map[string]any) { s["nico"] = ipxe("\n#!ipxe\nchain ${boot-loader}\n") }, invalid: true},
		{name: "command on the magic line", change: func(s map[string]any) { s["nico"] = ipxe("#!ipxe chain ${boot-loader}\n") }, invalid: true},
		{name: "magic glued to the next word", change: func(s map[string]any) { s["nico"] = ipxe("#!ipxecho\n") }, invalid: true},
		{name: "magic line with no command", change: func(s map[string]any) { s["nico"] = ipxe("#!ipxe\n") }, invalid: true},
		{name: "magic line with a Windows newline", change: func(s map[string]any) { s["nico"] = ipxe("#!ipxe\r\nboot\r\n") }},
		{name: "template action left open", change: func(s map[string]any) { s["nico"] = ipxe("#!ipxe\nchain {{ .DiskSelectionStrategy\n") }, invalid: true},
		{name: "template action never opened", change: func(s map[string]any) { s["nico"] = ipxe("#!ipxe\nchain .DiskSelectionStrategy }}\n") }, invalid: true},
		// Counting delimiters is not parsing: a template that renders to
		// nothing useful is still admitted, and fails where it is rendered.
		{name: "template action referencing an unknown value", change: func(s map[string]any) { s["nico"] = ipxe("#!ipxe\nchain {{ .Nonexistent }}\n") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			schema.check(t, tt.invalid, mutate(t, "machineimages", tt.change), nil)
		})
	}
}

// A machine image carries no lifecycle state, so nothing in its spec may change.
func TestMachineImageSpecImmutable(t *testing.T) {
	schema := loadResourceSchema(t, "machineimages")
	published := baseResource(t, "machineimages")
	schema.admit(t, published, nil)

	for _, tt := range []struct {
		name    string
		change  func(spec map[string]any)
		invalid bool
	}{
		{name: "unchanged", change: func(map[string]any) {}},
		{name: "change architecture", change: func(s map[string]any) { s["architecture"] = "arm64" }, invalid: true},
		{name: "change build date", change: func(s map[string]any) { s["buildDate"] = "2026-04-01" }, invalid: true},
		{name: "change iPXE script", change: func(s map[string]any) { s["nico"] = ipxe("#!ipxe\nexit\n") }, invalid: true},
		{name: "remove variant", change: func(s map[string]any) { delete(s, "variant") }, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			schema.check(t, tt.invalid, mutate(t, "machineimages", tt.change), runtime.DeepCopyJSON(published))
		})
	}
}

func TestReleaseAdmission(t *testing.T) {
	for _, resource := range releaseResources {
		t.Run(resource, func(t *testing.T) {
			schema := loadResourceSchema(t, resource)
			for _, tt := range []struct {
				name    string
				change  func(spec map[string]any)
				invalid bool
			}{
				{name: "minimal release", change: func(map[string]any) {}},
				{name: "missing image reference", change: func(s map[string]any) { delete(s, "machineImageRef") }, invalid: true},
				{name: "image reference not a DNS name", change: func(s map[string]any) { s["machineImageRef"] = map[string]any{"name": "Not A Name"} }, invalid: true},
				{name: "missing machine init", change: func(s map[string]any) { delete(s, "machineInit") }, invalid: true},
				{name: "empty user data", change: func(s map[string]any) { s["machineInit"] = userData("") }, invalid: true},
				{name: "blank user data", change: func(s map[string]any) { s["machineInit"] = userData("  \n ") }, invalid: true},
				{name: "user data without a cloud-init header", change: func(s map[string]any) { s["machineInit"] = userData("hostname: {{ .Hostname }}\n") }, invalid: true},
				{name: "include file user data", change: func(s map[string]any) { s["machineInit"] = userData("#include\nhttp://example.test/init\n") }},
				{name: "multipart user data", change: func(s map[string]any) {
					s["machineInit"] = userData("Content-Type: multipart/mixed; boundary=BOUNDARY\n")
				}},
				{name: "user data template action left open", change: func(s map[string]any) { s["machineInit"] = userData("#cloud-config\nhostname: {{ .Hostname\n") }, invalid: true},
				{name: "user data template action never opened", change: func(s map[string]any) { s["machineInit"] = userData("#cloud-config\nhostname: .Hostname }}\n") }, invalid: true},
				// User data is not required to be cloud-config: cloud-init also
				// accepts a script, an include file and MIME multipart.
				{name: "shell script user data", change: func(s map[string]any) {
					s["machineInit"] = userData("#!/bin/bash\nhostnamectl set-hostname {{ .Hostname }}\n")
				}},
			} {
				t.Run(tt.name, func(t *testing.T) {
					schema.check(t, tt.invalid, mutate(t, resource, tt.change), nil)
				})
			}
		})
	}
}

// Lifecycle is the mutable boundary: deprecating a release must succeed while
// every field describing what it provisions must not change.
func TestReleaseImmutabilityBoundary(t *testing.T) {
	for _, resource := range releaseResources {
		t.Run(resource, func(t *testing.T) {
			schema := loadResourceSchema(t, resource)
			published := baseResource(t, resource)
			schema.admit(t, published, nil)

			for _, tt := range []struct {
				name    string
				change  func(spec map[string]any)
				invalid bool
			}{
				{name: "unchanged", change: func(map[string]any) {}},
				{name: "deprecate", change: func(s map[string]any) { s["lifecycle"] = map[string]any{"deprecated": true} }},
				{name: "undeprecate", change: func(s map[string]any) { s["lifecycle"] = map[string]any{"deprecated": false} }},
				{name: "change image reference", change: func(s map[string]any) { s["machineImageRef"] = map[string]any{"name": "other-image"} }, invalid: true},
				{name: "change user data", change: func(s map[string]any) { s["machineInit"] = userData("#cloud-config\nhostname: other\n") }, invalid: true},
			} {
				t.Run(tt.name, func(t *testing.T) {
					schema.check(t, tt.invalid, mutate(t, resource, tt.change), runtime.DeepCopyJSON(published))
				})
			}
		})
	}
}

// mutableReleaseField is the single field a published release may still change.
const mutableReleaseField = "lifecycle"

// specSchema returns the spec property of a generated CRD, before conversion
// and defaulting, so the tests below can read the markers as controller-gen
// emitted them.
func specSchema(t *testing.T, resource string) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	crd := loadCRD(t, resource)
	spec, ok := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	if !ok {
		t.Fatalf("%s has no spec schema", resource)
	}
	return spec
}

func hasImmutabilityRule(rules []apiextensionsv1.ValidationRule) bool {
	for _, rule := range rules {
		if rule.Rule == "self == oldSelf" {
			return true
		}
	}
	return false
}

// The immutability convention is enforced here rather than left to review. A
// field added to a release spec without its own rule, or an optional field
// added without the presence rule that a field-level rule cannot provide,
// fails this test instead of shipping quietly mutable.
func TestReleaseImmutabilityConventionIsComplete(t *testing.T) {
	for _, resource := range releaseResources {
		t.Run(resource, func(t *testing.T) {
			spec := specSchema(t, resource)
			// A field-level rule is enough only where the field is always
			// there to carry it: required, or defaulted before validation.
			alwaysPresent := make(map[string]bool, len(spec.Required))
			for _, name := range spec.Required {
				alwaysPresent[name] = true
			}
			for name, property := range spec.Properties {
				if property.Default != nil {
					alwaysPresent[name] = true
				}
			}
			var specRules string
			for _, rule := range spec.XValidations {
				specRules += rule.Rule + "\n"
			}

			if _, ok := spec.Properties[mutableReleaseField]; !ok {
				t.Fatalf("spec has no %s field", mutableReleaseField)
			}

			for name, property := range spec.Properties {
				t.Run(name, func(t *testing.T) {
					if name == mutableReleaseField {
						if hasImmutabilityRule(property.XValidations) {
							t.Errorf("spec.%s must stay mutable: deprecating a release is the one edit a published release allows", name)
						}
						return
					}
					if !hasImmutabilityRule(property.XValidations) {
						t.Errorf(`spec.%s has no immutability rule; add +kubebuilder:validation:XValidation:rule="self == oldSelf" to the field`, name)
					}
					if alwaysPresent[name] {
						return
					}
					// A field-level rule is skipped while the field is absent,
					// so a field that can be omitted also needs its presence
					// pinned at the spec level, where the rule always runs.
					presence := fmt.Sprintf("has(self.%s) == has(oldSelf.%s)", name, name)
					if !strings.Contains(specRules, presence) {
						t.Errorf("spec.%s can be omitted and is immutable, so the spec needs the rule %q", name, presence)
					}
				})
			}
		})
	}
}

// A machine image has no lifecycle, so it takes the stricter whole-spec rule
// and needs none of the per-field bookkeeping above.
func TestMachineImageSpecRuleCoversEverything(t *testing.T) {
	spec := specSchema(t, "machineimages")
	if !hasImmutabilityRule(spec.XValidations) {
		t.Error(`machine image spec needs +kubebuilder:validation:XValidation:rule="self == oldSelf" on the spec type`)
	}
	if _, ok := spec.Properties[mutableReleaseField]; ok {
		t.Errorf("machine image spec gained a %s field, so the whole-spec rule is now wrong", mutableReleaseField)
	}
}

// Defaulting has to leave lifecycle present and false, because the immutability
// rule and the resolver both read it without checking for its absence.
func TestReleaseLifecycleDefaults(t *testing.T) {
	for _, resource := range releaseResources {
		t.Run(resource, func(t *testing.T) {
			obj := baseResource(t, resource)
			loadResourceSchema(t, resource).admit(t, obj, nil)

			lifecycle, ok := specOf(t, obj)["lifecycle"].(map[string]any)
			if !ok {
				t.Fatal("lifecycle default missing")
			}
			if lifecycle["deprecated"] != false {
				t.Fatalf("unexpected lifecycle default: %#v", lifecycle)
			}
		})
	}
}
