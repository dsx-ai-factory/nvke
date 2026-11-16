// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	schemacel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/listtype"
	apivalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// These tests run Kubernetes' schema and CEL validators against the CRDs the
// NVKE controller's chart ships. They do not simulate authorization, admission
// webhooks or the status subresource.

// crdDirectory holds the generated CRDs, which controller-gen writes from the
// types in this package.
const crdDirectory = "../../deploy/charts/nvke-crds/bases/"

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
	data, err := os.ReadFile(crdDirectory + GroupName + "_" + resource + ".yaml")
	require.NoError(t, err)
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.UnmarshalStrict(data, &crd))
	return crd
}

func loadResourceSchema(t *testing.T, resource string) resourceSchema {
	t.Helper()
	crd := loadCRD(t, resource)
	apiextensionsv1.SetDefaults_CustomResourceDefinition(&crd)

	var internal apiextensions.CustomResourceDefinition
	require.NoError(t, apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&crd, &internal, nil))
	// This also checks structural rules, default validity and CEL compile/cost limits.
	errs := crdvalidation.ValidateCustomResourceDefinition(context.Background(), &internal)
	require.Empty(t, errs, "invalid generated CRD")

	schema := storageVersionSchema(t, internal)
	structural, err := structuralschema.NewStructural(schema)
	require.NoError(t, err)
	openAPI, _, err := apivalidation.NewSchemaValidator(schema)
	require.NoError(t, err)
	return resourceSchema{
		structural: structural,
		openAPI:    openAPI,
		// Nil when the schema carries no CEL rules, which validate allows for.
		cel: schemacel.NewValidator(structural, true, celconfig.PerCallLimit),
	}
}

func storageVersionSchema(t *testing.T, crd apiextensions.CustomResourceDefinition) *apiextensions.JSONSchemaProps {
	t.Helper()
	require.Len(t, crd.Spec.Versions, 1, "expected one version")
	version := crd.Spec.Versions[0]
	require.True(t, version.Served && version.Storage && version.Name == GroupVersion.Version, "expected one served storage version: %s", GroupVersion.Version)
	validation := version.Schema
	if validation == nil {
		// Conversion lifts a schema shared by all versions to spec.validation.
		validation = crd.Spec.Validation
	}
	require.False(t, validation == nil || validation.OpenAPIV3Schema == nil, "generated CRD has no validation schema")
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

// Minimal resources that pass admission: every required field and nothing else.
var baseResources = map[string]string{
	"machineimages": `{
		"apiVersion": "internal.nvke.nvidia.com/v1",
		"kind": "MachineImage",
		"metadata": {"name": "nvke-nico-vms-24.04-amd64-fde"},
		"spec": {
			"operatingSystem": {"family": "ubuntu", "version": "24.04"},
			"architecture": "amd64",
			"variant": "fde",
			"buildDate": "2026-03-23",
			"nico": {"ipxeScript": "#!ipxe\nchain ${boot-loader} disk_strategy={{ .DiskSelectionStrategy }}\n"}
		}
	}`,
	"clusterreleases": `{
		"apiVersion": "internal.nvke.nvidia.com/v1",
		"kind": "ClusterRelease",
		"metadata": {"name": "nvke-nico-vms-24.04-amd64-fde"},
		"spec": {
			"machineImageRef": {"name": "nvke-nico-vms-24.04-amd64-fde"},
			"machineInit": {"userDataTemplate": "#cloud-config\nhostname: {{ .Hostname }}\n"}
		}
	}`,
	"nodepoolreleases": `{
		"apiVersion": "internal.nvke.nvidia.com/v1",
		"kind": "NodePoolRelease",
		"metadata": {"name": "nvke-nico-vms-24.04-amd64-fde"},
		"spec": {
			"machineImageRef": {"name": "nvke-nico-vms-24.04-amd64-fde"},
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
	require.True(t, ok, "no base resource for %q", resource)
	return decodeJSON[map[string]any](t, []byte(data))
}

func specOf(t *testing.T, obj map[string]any) map[string]any {
	t.Helper()
	spec, ok := obj["spec"].(map[string]any)
	require.True(t, ok, "resource has no spec: %#v", obj)
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
	require.NoError(t, json.Unmarshal(data, &value))
	return value
}

func encodeJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
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
	for _, resource := range []string{"machineimages", "clusterreleases", "nodepoolreleases"} {
		t.Run(resource, func(t *testing.T) {
			loadResourceSchema(t, resource).admit(t, baseResource(t, resource), nil)
		})
	}
}

// What the Go types emit must be admissible by the generated CRDs, and must
// survive an encode and decode unchanged.
func TestTypedResourcesRoundTrip(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, AddToScheme(scheme))
	codecs := serializer.NewCodecFactory(scheme)
	machineInit := MachineInit{UserDataTemplate: "#cloud-config\nhostname: {{ .Hostname }}\n"}
	name := metav1.ObjectMeta{Name: "nvke-nico-vms-24.04-amd64-fde"}
	imageRef := MachineImageReference{Name: "nvke-nico-vms-24.04-amd64-fde"}

	for _, tt := range []struct {
		resource string
		kind     string
		object   runtime.Object
	}{
		{
			resource: "machineimages", kind: "MachineImage",
			object: &MachineImage{
				ObjectMeta: name,
				Spec: MachineImageSpec{
					OperatingSystem: OperatingSystem{Family: "ubuntu", Version: "24.04"},
					Architecture:    "amd64",
					Variant:         "fde",
					BuildDate:       "2026-03-23",
					NICo:            NICoBoot{IPXEScript: "#!ipxe\nboot\n"},
				},
			},
		},
		{
			resource: "clusterreleases", kind: "ClusterRelease",
			object: &ClusterRelease{
				ObjectMeta: name,
				Spec:       ClusterReleaseSpec{MachineImageRef: imageRef, MachineInit: machineInit},
			},
		},
		{
			resource: "nodepoolreleases", kind: "NodePoolRelease",
			object: &NodePoolRelease{
				ObjectMeta: name,
				Spec:       NodePoolReleaseSpec{MachineImageRef: imageRef, MachineInit: machineInit, Profile: "ubuntu", Tag: "24.04"},
			},
		},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			encoded, err := runtime.Encode(codecs.LegacyCodec(GroupVersion), tt.object)
			require.NoError(t, err)
			decoded, gvk, err := codecs.UniversalDeserializer().Decode(encoded, nil, nil)
			require.NoError(t, err)
			require.Equal(t, GroupVersion.WithKind(tt.kind), *gvk)
			reencoded, err := runtime.Encode(codecs.LegacyCodec(GroupVersion), decoded)
			require.NoError(t, err)
			require.JSONEq(t, string(encoded), string(reencoded), "round trip changed the object")

			value := decodeJSON[map[string]any](t, encoded)
			require.NotContains(t, value, "status", "unobserved status must be omitted")
			loadResourceSchema(t, tt.resource).admit(t, value, nil)
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
		{name: "variant with dots", change: func(s map[string]any) { s["variant"] = "na-0.1.29" }},
		{name: "uppercase variant", change: func(s map[string]any) { s["variant"] = "FDE" }, invalid: true},
		{name: "variant with an underscore", change: func(s map[string]any) { s["variant"] = "na_0" }, invalid: true},
		{name: "variant ending in a dot", change: func(s map[string]any) { s["variant"] = "na-0." }, invalid: true},
		{name: "variant starting with a dot", change: func(s map[string]any) { s["variant"] = ".na" }, invalid: true},
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

func TestNodePoolReleaseProfile(t *testing.T) {
	schema := loadResourceSchema(t, "nodepoolreleases")
	legacy := baseResource(t, "nodepoolreleases")
	published := mutate(t, "nodepoolreleases", func(spec map[string]any) {
		spec["profile"], spec["tag"] = "ubuntu", "24.04"
	})
	schema.admit(t, legacy, nil)
	schema.admit(t, published, nil)
	schema.admit(t, runtime.DeepCopyJSON(published), published)
	schema.check(t, true, published, legacy)

	for _, entry := range []fieldCase{
		{name: "invalid profile", path: []string{"spec", "profile"}, value: "Not A Name", invalid: true},
		{name: "empty tag", path: []string{"spec", "tag"}, value: "", invalid: true},
		{name: "invalid tag", path: []string{"spec", "tag"}, value: "repo:tag", invalid: true},
		{name: "oversized tag", path: []string{"spec", "tag"}, value: strings.Repeat("a", 129), invalid: true},
	} {
		t.Run(entry.name, func(t *testing.T) {
			candidate := runtime.DeepCopyJSON(published)
			entry.apply(candidate)
			schema.check(t, entry.invalid, candidate, nil)
		})
	}
	for _, name := range []string{"profile", "tag"} {
		t.Run(name+" is immutable", func(t *testing.T) {
			candidate := runtime.DeepCopyJSON(published)
			specOf(t, candidate)[name] = "other"
			schema.check(t, true, candidate, published)
			delete(specOf(t, candidate), name)
			schema.check(t, true, candidate, published)
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

// Deprecating a release goes through the Go types, so a decode/edit/encode
// cycle must not itself look like a change to the immutable content. Without
// this, the spec-level rule could pass its own unit tests and still reject
// every write the controller or nvkectl makes.
func TestLifecycleEditRoundTripsThroughGoTypes(t *testing.T) {
	for _, resource := range releaseResources {
		t.Run(resource, func(t *testing.T) {
			schema := loadResourceSchema(t, resource)
			published := baseResource(t, resource)
			schema.admit(t, published, nil)

			var updated map[string]any
			switch resource {
			case "clusterreleases":
				release := decodeJSON[ClusterRelease](t, encodeJSON(t, published))
				release.Spec.Lifecycle.Deprecated = true
				updated = decodeJSON[map[string]any](t, encodeJSON(t, release))
			default:
				release := decodeJSON[NodePoolRelease](t, encodeJSON(t, published))
				release.Spec.Lifecycle.Deprecated = true
				updated = decodeJSON[map[string]any](t, encodeJSON(t, release))
			}
			schema.admit(t, updated, published)

			// The same round trip that permits the deprecation must still
			// refuse a content edit, so the check above is not vacuous.
			specOf(t, updated)["machineImageRef"] = map[string]any{"name": "other-image"}
			schema.check(t, true, updated, published)
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
	require.True(t, ok, "%s has no spec schema", resource)
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

			require.Contains(t, spec.Properties, mutableReleaseField, "spec has no %s field", mutableReleaseField)

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
			require.True(t, ok, "lifecycle default missing")
			require.Equal(t, false, lifecycle["deprecated"], "unexpected lifecycle default: %#v", lifecycle)
		})
	}
}
