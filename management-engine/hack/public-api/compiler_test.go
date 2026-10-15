// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Compiler tests guard exposure decisions and reproducible public API generation.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// fixture loads the reviewed schema and exposure policy.
// Tests mutate copies so their cases stay grounded in the real contract.
func fixture(t *testing.T) ([]byte, []byte) {
	t.Helper()
	crd, err := os.ReadFile("../../config/crd/bases/internal.nvke.nvidia.com_nvkeclusters.yaml")
	require.NoError(t, err)
	policy, err := os.ReadFile("../../public/tenant/apis/cluster/v1/exposure.json")
	require.NoError(t, err)
	return crd, policy
}

// TestReviewedContractReproduces checks the committed generated artifacts.
// This catches drift between reviewed inputs and delivered output.
func TestReviewedContractReproduces(t *testing.T) {
	require.NoError(t, run("../..", true, false))
}

// policyField finds a fixture field by its internal path.
// Named lookup keeps mutation cases independent of policy ordering.
func policyField(t *testing.T, p *policy, path string) *field {
	t.Helper()
	for i := range p.Fields {
		if p.Fields[i].Path == path {
			return &p.Fields[i]
		}
	}
	t.Fatalf("fixture is missing public field %q", path)
	return nil
}

// publicAccessDefault finds the fixture's private access default.
// Tests can change this admission requirement without relying on its position.
func publicAccessDefault(t *testing.T, p *policy) *defaultValue {
	t.Helper()
	const path = "apiServerAccess.publicAccessEnabled"
	for i := range p.Defaults {
		if p.Defaults[i].Path == path {
			return &p.Defaults[i]
		}
	}
	t.Fatalf("fixture is missing private default %q", path)
	return nil
}

// TestPolicyFailsClosed rejects invalid exposure policies and normalizes valid ordering.
// This prevents ambiguous policy input from widening or changing the generated API.
func TestPolicyFailsClosed(t *testing.T) {
	crd, data := fixture(t)
	tests := []struct {
		name, diagnostic string
		mutate           func(*testing.T, *policy)
	}{
		{"unknown path", "exposure path missing from internal schema", func(t *testing.T, p *policy) {
			policyField(t, p, "release").Path = "private"
		}},
		{"duplicate path", "duplicate exposure path", func(t *testing.T, p *policy) {
			p.Fields = append(p.Fields, *policyField(t, p, "release"))
		}},
		{"duplicate tag", "duplicate sibling tag or public name", func(t *testing.T, p *policy) {
			policyField(t, p, "controlPlaneClass").Tag = policyField(t, p, "release").Tag
		}},
		{"reserved tag", "invalid field path or protobuf tag", func(t *testing.T, p *policy) {
			policyField(t, p, "release").Tag = 19000
		}},
		{"out of range tag", "invalid field path or protobuf tag", func(t *testing.T, p *policy) {
			policyField(t, p, "release").Tag = 536870912
		}},
		{"missing parent", "requires an explicit parent policy", func(_ *testing.T, p *policy) {
			p.Fields = slices.DeleteFunc(p.Fields, func(f field) bool { return f.Path == "release" })
		}},
		{"new audience", "unsupported or duplicate access", func(t *testing.T, p *policy) {
			policyField(t, p, "release").Access = []string{"operator"}
		}},
		{"unexposed parent mode", "parent does not expose", func(t *testing.T, p *policy) {
			policyField(t, p, "release").Access = []string{"read"}
		}},
		{"empty access", "missing field access", func(t *testing.T, p *policy) {
			policyField(t, p, "release").Access = nil
		}},
		{"duplicate access", "unsupported or duplicate access", func(t *testing.T, p *policy) {
			policyField(t, p, "release").Access = []string{"read", "read"}
		}},
		{"identifier injection", "invalid field path or protobuf tag", func(t *testing.T, p *policy) {
			policyField(t, p, "controlPlaneClass").Path = "controlPlaneClass; import evil"
		}},
		{"default injection", "invalid, duplicate, or create-writable default path", func(t *testing.T, p *policy) {
			publicAccessDefault(t, p).Path = "apiServerAccess; import evil"
		}},
		{"wrong default type", "is not a stored string", func(t *testing.T, p *policy) {
			publicAccessDefault(t, p).Value = json.RawMessage(`"false"`)
		}},
		{"unsupported default type", "must be an explicit string or boolean", func(t *testing.T, p *policy) {
			publicAccessDefault(t, p).Value = json.RawMessage(`4`)
		}},
		{"duplicate default", "invalid, duplicate, or create-writable default path", func(t *testing.T, p *policy) {
			p.Defaults = append(p.Defaults, *publicAccessDefault(t, p))
		}},
		{"writable default", "invalid, duplicate, or create-writable default path", func(_ *testing.T, p *policy) {
			p.Defaults = append(p.Defaults, defaultValue{Path: "release.channel.name", Value: json.RawMessage(`"stable"`)})
		}},
		{"input policy on read-only field", "requires a writable operation", func(t *testing.T, p *policy) {
			policyField(t, p, "clusterNetwork.pods.cidrBlocks").Access = []string{modeRead}
		}},
		{"input collection limit on scalar", "requires an array", func(t *testing.T, p *policy) {
			policyField(t, p, "controlPlaneClass").Input = &inputPolicy{MaxItems: new(int64(1))}
		}},
		{"input weakens stored limit", "must narrow the stored bounds", func(t *testing.T, p *policy) {
			policyField(t, p, "clusterNetwork.pods.cidrBlocks").Input.MaxItems = new(int64(3))
		}},
		{"input contradicts stored minimum", "must narrow the stored bounds", func(t *testing.T, p *policy) {
			policyField(t, p, "clusterNetwork.pods.cidrBlocks").Input.MaxItems = new(int64(0))
		}},
		{"empty input policy", "input policy on controlPlaneClass is empty", func(t *testing.T, p *policy) {
			policyField(t, p, "controlPlaneClass").Input = &inputPolicy{}
		}},
	}
	baseline, err := compile(crd, data)
	require.NoError(t, err)
	want, err := render(baseline)
	require.NoError(t, err)
	t.Run("reordered", func(t *testing.T) {
		var input policy
		require.NoError(t, json.Unmarshal(data, &input))
		slices.SortFunc(input.Fields, func(a, b field) int { return strings.Compare(a.Path, b.Path) })
		slices.Reverse(input.Defaults)
		ordered, err := json.Marshal(input)
		require.NoError(t, err)
		model, err := compile(crd, ordered)
		require.NoError(t, err)
		got, err := render(model)
		require.NoError(t, err)
		require.Equal(t, want, got, "policy ordering must not affect generated artifacts")
	})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var selected policy
			require.NoError(t, json.Unmarshal(data, &selected))
			test.mutate(t, &selected)
			changed, err := json.Marshal(selected)
			require.NoError(t, err)
			_, err = compile(crd, changed)
			require.ErrorContains(t, err, test.diagnostic)
		})
	}
	_, err = compile(crd, append(data, []byte(" {}")...))
	require.Error(t, err)
	_, err = compile(crd, []byte(`{"version":"v1","typo":true,"fields":[]}`))
	require.Error(t, err)
}

// TestUnreviewedSchemaShapesFailClosed rejects shapes with undefined mapping behavior.
// Generated clients must not imply support for values or presence the service cannot preserve.
func TestUnreviewedSchemaShapesFailClosed(t *testing.T) {
	tests := map[string]apiextensionsv1.JSONSchemaProps{
		"numeric":             {Type: "number"},
		"nullable":            {Type: "string", Nullable: true},
		"union":               {Type: "string", OneOf: []apiextensionsv1.JSONSchemaProps{{Type: "string"}}},
		"map":                 {Type: "object", AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{Allows: true}},
		"enum":                {Type: "string", Enum: []apiextensionsv1.JSON{{Raw: []byte(`"one"`)}}},
		"empty-only string":   {Type: "string", MaxLength: new(int64(0))},
		"inconsistent bounds": {Type: "string", MinLength: new(int64(2)), MaxLength: new(int64(1))},
		"list without topology": {Type: "array", Items: &apiextensionsv1.JSONSchemaPropsOrArray{
			Schema: &apiextensionsv1.JSONSchemaProps{Type: "string"},
		}},
	}
	for name, shape := range tests {
		t.Run(name, func(t *testing.T) { require.Error(t, supportedSchema(&shape)) })
	}
	crdData, policyData := fixture(t)
	for _, test := range []struct {
		name                          string
		access, childAccess           []string
		objectRequired, childRequired bool
		defaultChild, reject          bool
	}{
		{name: "optional writable object", access: []string{modeCreate}, childAccess: []string{modeCreate}, reject: true},
		{name: "required object with optional children", access: []string{modeCreate}, childAccess: []string{modeCreate},
			objectRequired: true, reject: true},
		{name: "required child unavailable to replacement",
			access: []string{modeRead, modeReplace}, childAccess: []string{modeRead},
			childRequired: true, reject: true},
		{name: "private default does not establish public presence",
			access: []string{modeCreate, modeRead}, childAccess: []string{modeRead},
			childRequired: true, defaultChild: true, reject: true},
		{name: "optional read-only object", access: []string{modeRead}, childAccess: []string{modeRead}},
		{name: "writable object with required child",
			access: []string{modeCreate}, childAccess: []string{modeCreate}, childRequired: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var crd apiextensionsv1.CustomResourceDefinition
			require.NoError(t, yaml.UnmarshalStrict(crdData, &crd))
			spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
			object := apiextensionsv1.JSONSchemaProps{
				Type: schemaObject, Properties: map[string]apiextensionsv1.JSONSchemaProps{"note": {Type: schemaString}},
			}
			if test.childRequired {
				object.Required = []string{"note"}
			}
			if test.objectRequired {
				spec.Required = append(spec.Required, "futureOptions")
			}
			spec.Properties["futureOptions"] = object
			crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = spec
			changedCRD, err := yaml.Marshal(crd)
			require.NoError(t, err)
			var exposure policy
			require.NoError(t, json.Unmarshal(policyData, &exposure))
			exposure.Fields = append(exposure.Fields,
				field{Path: "futureOptions", Tag: 4, Access: test.access},
				field{Path: "futureOptions.note", Tag: 1, Access: test.childAccess},
			)
			if test.defaultChild {
				exposure.Defaults = append(exposure.Defaults,
					defaultValue{Path: "futureOptions.note", Value: json.RawMessage(`"fixed"`)})
			}
			changedPolicy, err := json.Marshal(exposure)
			require.NoError(t, err)
			_, err = compile(changedCRD, changedPolicy)
			if test.reject {
				require.ErrorContains(t, err, "writable object futureOptions requires a required")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestPrivateSchemaGrowthDoesNotExpandProjection adds unselected schema fields.
// Public artifacts must remain unchanged until exposure is explicitly reviewed.
func TestPrivateSchemaGrowthDoesNotExpandProjection(t *testing.T) {
	crdData, policyData := fixture(t)
	before, err := compile(crdData, policyData)
	require.NoError(t, err)
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.UnmarshalStrict(crdData, &crd))
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	spec.Properties["privateNewField"] = apiextensionsv1.JSONSchemaProps{
		Type: "string", Description: "PRIVATE-DESCRIPTION", MinLength: new(int64(7)),
		Pattern: "PRIVATE-PATTERN", Default: &apiextensionsv1.JSON{Raw: []byte(`"PRIVATE-DEFAULT"`)},
		XValidations: []apiextensionsv1.ValidationRule{{Rule: "self != 'PRIVATE-CEL'"}},
	}
	spec.Properties["privateOptionalObject"] = apiextensionsv1.JSONSchemaProps{
		Type: "object", Required: []string{"requiredOnlyWhenPresent"},
		Properties: map[string]apiextensionsv1.JSONSchemaProps{"requiredOnlyWhenPresent": {Type: "string"}},
	}
	release := spec.Properties["release"]
	release.Properties["privateNewChild"] = apiextensionsv1.JSONSchemaProps{Type: "string"}
	spec.Properties["release"] = release
	crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = spec
	changed, err := yaml.Marshal(crd)
	require.NoError(t, err)
	after, err := compile(changed, policyData)
	require.NoError(t, err)
	want, err := render(before)
	require.NoError(t, err)
	got, err := render(after)
	require.NoError(t, err)
	require.Equal(t, want, got)
	lock, err := verifyLock(before, nil, true)
	require.NoError(t, err)
	_, err = verifyLock(after, lock, false)
	require.NoError(t, err)
}

// TestAncestorAdmissionDriftRequiresReview changes rules above exposed fields.
// Those rules can alter accepted requests even when the selected fields are unchanged.
func TestAncestorAdmissionDriftRequiresReview(t *testing.T) {
	for _, ancestor := range []string{"resource", "spec"} {
		t.Run(ancestor, func(t *testing.T) {
			crdData, policyData := fixture(t)
			before, err := compile(crdData, policyData)
			require.NoError(t, err)
			lock, err := verifyLock(before, nil, true)
			require.NoError(t, err)
			var crd apiextensionsv1.CustomResourceDefinition
			require.NoError(t, yaml.UnmarshalStrict(crdData, &crd))
			resource := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
			if ancestor == "spec" {
				spec := resource.Properties["spec"]
				spec.XValidations = append(spec.XValidations, apiextensionsv1.ValidationRule{
					Rule: "self.controlPlaneClass.startsWith('x')",
				})
				resource.Properties["spec"] = spec
			} else {
				resource.XValidations = append(resource.XValidations, apiextensionsv1.ValidationRule{
					Rule: "self.spec.controlPlaneClass.startsWith('x')",
				})
			}
			changed, err := yaml.Marshal(crd)
			require.NoError(t, err)
			after, err := compile(changed, policyData)
			require.NoError(t, err)
			_, err = verifyLock(after, lock, false)
			require.ErrorContains(t, err, "changed entry $"+ancestor)
			registered, err := verifyLock(after, lock, true)
			require.NoError(t, err)
			_, err = verifyLock(after, registered, false)
			require.NoError(t, err)
		})
	}
}

// TestRequiredPrivateSiblingDriftRequiresReview adds required unexposed fields.
// Generation must reject requests whose private requirements cannot be satisfied.
func TestRequiredPrivateSiblingDriftRequiresReview(t *testing.T) {
	for _, ancestor := range []string{"spec", "release", "apiServerAccess"} {
		t.Run(ancestor, func(t *testing.T) {
			crdData, policyData := fixture(t)
			var crd apiextensionsv1.CustomResourceDefinition
			require.NoError(t, yaml.UnmarshalStrict(crdData, &crd))
			resource := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
			spec := resource.Properties["spec"]
			if ancestor == "spec" {
				spec.Required = append(spec.Required, "authentication")
			} else {
				release := spec.Properties[ancestor]
				release.Properties["privateRequired"] = apiextensionsv1.JSONSchemaProps{Type: "string"}
				release.Required = append(release.Required, "privateRequired")
				spec.Properties[ancestor] = release
			}
			resource.Properties["spec"] = spec
			changed, err := yaml.Marshal(crd)
			require.NoError(t, err)
			_, err = compile(changed, policyData)
			require.ErrorContains(t, err, "required stored field")
		})
	}
}

// TestRegistryCompatibility requires acknowledgement of unpublished contract changes.
// Explicit updates must record breaking changes as well as additions.
func TestRegistryCompatibility(t *testing.T) {
	crd, data := fixture(t)
	baseline, err := compile(crd, data)
	require.NoError(t, err)
	lock, err := verifyLock(baseline, nil, true)
	require.NoError(t, err)
	for _, test := range []struct {
		name, diagnostic string
		mutate           func(*testing.T, *model, map[string]record)
	}{
		{name: "removed field", diagnostic: "removed entry",
			mutate: func(_ *testing.T, m *model, _ map[string]record) {
				m.nodes = m.nodes[1:]
			}},
		{name: "changed tag", diagnostic: "changed entry",
			mutate: func(_ *testing.T, m *model, _ map[string]record) {
				m.nodes[0].Tag++
			}},
		{name: "changed requiredness", diagnostic: "changed entry",
			mutate: func(_ *testing.T, m *model, _ map[string]record) {
				m.nodes[0].Required = !m.nodes[0].Required
			}},
		{name: "changed access", diagnostic: "changed entry",
			mutate: func(_ *testing.T, m *model, _ map[string]record) {
				m.nodes[0].Access = []string{modeRead}
			}},
		{name: "changed schema", diagnostic: "changed entry",
			mutate: func(_ *testing.T, m *model, _ map[string]record) {
				m.nodes[0].Schema.Nullable = true
			}},
		{name: "changed input narrowing", diagnostic: "changed entry",
			mutate: func(_ *testing.T, m *model, _ map[string]record) {
				for _, field := range m.nodes {
					if field.Path == "clusterNetwork.pods.cidrBlocks" {
						field.Input.MaxItems = new(int64(2))
					}
				}
			}},
		{name: "changed private default", diagnostic: "changed entry",
			mutate: func(_ *testing.T, m *model, _ map[string]record) {
				path := "$default:apiServerAccess.publicAccessEnabled"
				changed := m.createAdmission[path]
				changed.Default = json.RawMessage(`true`)
				m.createAdmission[path] = changed
			}},
		{name: "required create addition", diagnostic: "new required create field",
			mutate: func(_ *testing.T, m *model, _ map[string]record) {
				m.nodes = append(m.nodes, &node{
					Path: "future", Tag: 4, Access: []string{modeCreate}, Required: true,
					Schema: apiextensionsv1.JSONSchemaProps{Type: schemaString},
				})
			}},
		{name: "replacement addition", diagnostic: "new replacement field",
			mutate: func(_ *testing.T, _ *model, previous map[string]record) {
				// Simulate an older contract without the optional channel version.
				delete(previous, "release.channel.version")
			}},
		{name: "read-only addition", diagnostic: "added entry",
			mutate: func(t *testing.T, m *model, _ map[string]record) {
				var exposure policy
				require.NoError(t, json.Unmarshal(data, &exposure))
				exposure.Fields = append(exposure.Fields, field{
					Path: "clusterNetwork.serviceDomain", Tag: 3, Access: []string{modeRead},
				})
				changed, err := json.Marshal(exposure)
				require.NoError(t, err)
				after, err := compile(crd, changed)
				require.NoError(t, err)
				*m = *after
			}},
		{name: "unregistered contract", diagnostic: "public contract lock differs",
			mutate: func(_ *testing.T, _ *model, previous map[string]record) {
				clear(previous)
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed, err := compile(crd, data)
			require.NoError(t, err)
			var previous map[string]record
			require.NoError(t, json.Unmarshal(lock, &previous))
			test.mutate(t, changed, previous)
			previousLock, err := json.Marshal(previous)
			require.NoError(t, err)
			for _, update := range []bool{false, true} {
				t.Run(fmt.Sprintf("update=%t", update), func(t *testing.T) {
					registered, err := verifyLock(changed, previousLock, update)
					if !update {
						require.ErrorContains(t, err, test.diagnostic)
						return
					}
					require.NoError(t, err)
					_, err = verifyLock(changed, registered, false)
					require.NoError(t, err, "acknowledged changes must pass the normal check")
				})
			}
		})
	}
	t.Run("all changes are reported deterministically", func(t *testing.T) {
		var previous map[string]record
		require.NoError(t, json.Unmarshal(lock, &previous))
		delete(previous, "release.channel.version")
		previous["removed"] = record{}
		changed := previous["controlPlaneClass"]
		changed.Tag = 10
		previous["controlPlaneClass"] = changed
		data, err := json.Marshal(previous)
		require.NoError(t, err)
		_, err = verifyLock(baseline, data, false)
		require.ErrorContains(t, err, "changed entry controlPlaneClass\n- "+
			"new replacement field release.channel.version can clear values omitted by existing clients\n- "+
			"removed entry removed")
	})
	t.Run("malformed lock cannot be acknowledged", func(t *testing.T) {
		for _, update := range []bool{false, true} {
			_, err := verifyLock(baseline, []byte("{"), update)
			require.ErrorContains(t, err, "read compatibility registry")
		}
	})
	t.Run("update validates policy before writing", func(t *testing.T) {
		root := t.TempDir()
		policyPath := filepath.Join(root, contractDir, "exposure.json")
		require.NoError(t, writeOutput(policyPath, data, false))
		crdPath := filepath.Join(root, "config/crd/bases/internal.nvke.nvidia.com_nvkeclusters.yaml")
		require.NoError(t, writeOutput(crdPath, crd, false))
		require.NoError(t, run(root, false, true))
		outputs, err := render(baseline)
		require.NoError(t, err)
		outputs[filepath.Join(contractDir, "wire-lock.json")] = lock
		var exposure policy
		require.NoError(t, json.Unmarshal(data, &exposure))
		policyField(t, &exposure, "controlPlaneClass").Tag = policyField(t, &exposure, "release").Tag
		invalid, err := json.Marshal(exposure)
		require.NoError(t, err)
		require.NoError(t, writeOutput(policyPath, invalid, false))
		require.ErrorContains(t, run(root, false, true), "duplicate sibling tag or public name")
		for path, expected := range outputs {
			actual, err := os.ReadFile(filepath.Join(root, path)) // #nosec G304 -- Test-owned generated artifact.
			require.NoError(t, err)
			require.Equal(t, expected, actual, "invalid input must not change %s", path)
		}
	})
}

// TestCheckDoesNotWriteChangedArtifacts verifies that drift checking preserves files.
// A failed check must leave the evidence available for review.
func TestCheckDoesNotWriteChangedArtifacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact")
	require.NoError(t, os.WriteFile(path, []byte("original"), 0o600))
	require.Error(t, writeOutput(path, []byte("replacement"), true))
	current, err := os.ReadFile(path) // #nosec G304 -- Test-owned temporary artifact.
	require.NoError(t, err)
	require.Equal(t, "original", string(current))
}

// TestOptionalWritableListsFailClosed checks list exposure against presence requirements.
// Writes must not confuse omitted lists with explicitly empty values.
func TestOptionalWritableListsFailClosed(t *testing.T) {
	crdData, policyData := fixture(t)
	before, err := compile(crdData, policyData)
	require.NoError(t, err)
	lock, err := verifyLock(before, nil, true)
	require.NoError(t, err)
	for _, minimum := range []int64{0, 1} {
		for _, test := range []struct {
			name           string
			access         []string
			required       bool
			reject         bool
			readOnlyParent bool
		}{
			{name: "optional create", access: []string{modeCreate}, reject: true},
			{name: "optional replace", access: []string{modeReplace}, reject: true},
			{name: "optional read and create", access: []string{modeRead, modeCreate}, reject: true},
			{name: "optional read and replace", access: []string{modeRead, modeReplace}, reject: true},
			{name: "optional read", access: []string{modeRead}},
			{name: "required create", access: []string{modeCreate}, required: true},
			{name: "required create and replace", access: []string{modeCreate, modeReplace}, required: true},
			{name: "required read", access: []string{modeRead}, required: true, readOnlyParent: true},
		} {
			t.Run(fmt.Sprintf("%s/minItems=%d", test.name, minimum), func(t *testing.T) {
				var crd apiextensionsv1.CustomResourceDefinition
				require.NoError(t, yaml.UnmarshalStrict(crdData, &crd))
				spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
				release := spec.Properties["release"]
				parent, path := "release", "release.futureNames"
				if test.readOnlyParent {
					// A required read field can live under an optional read-only
					// object without changing the fields public create must supply.
					parent, path = "futureRead", "futureRead.futureNames"
					release = apiextensionsv1.JSONSchemaProps{
						Type: schemaObject, Properties: map[string]apiextensionsv1.JSONSchemaProps{},
					}
				}
				atomic := "atomic"
				release.Properties["futureNames"] = apiextensionsv1.JSONSchemaProps{
					Type: schemaArray, XListType: &atomic, MinItems: &minimum,
					Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &apiextensionsv1.JSONSchemaProps{Type: schemaString}},
				}
				if test.required {
					release.Required = append(release.Required, "futureNames")
				}
				spec.Properties[parent] = release
				crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = spec
				changedCRD, err := yaml.Marshal(crd)
				require.NoError(t, err)
				var exposure policy
				require.NoError(t, json.Unmarshal(policyData, &exposure))
				if test.readOnlyParent {
					exposure.Fields = append(exposure.Fields, field{Path: parent, Tag: 4, Access: test.access})
				}
				exposure.Fields = append(exposure.Fields, field{Path: path, Tag: 2, Access: test.access})
				changedPolicy, err := json.Marshal(exposure)
				require.NoError(t, err)
				after, err := compile(changedCRD, changedPolicy)
				if test.reject {
					require.ErrorContains(t, err, "optional writable array release.futureNames requires explicit presence semantics")
					return
				}
				if test.required && minimum == 0 {
					require.ErrorContains(t, err, "required array "+path+" needs positive minItems to preserve presence")
					return
				}
				require.NoError(t, err)
				if !test.required {
					_, err = verifyLock(after, lock, true)
					require.NoError(t, err, "an optional read-only addition remains compatible")
				}
			})
		}
	}
}

// TestSnakeCaseAcronyms checks word boundaries around capital runs.
// Public names must remain predictable without guessing boundaries between consecutive acronyms.
func TestSnakeCaseAcronyms(t *testing.T) {
	for internal, public := range map[string]string{
		"vpcID":                      "vpc_id",
		"vpcSubnetID":                "vpc_subnet_id",
		"cidrBlocks":                 "cidr_blocks",
		"apiURLValue":                "api_url_value",
		"ipv6CIDR":                   "ipv6_cidr",
		"name":                       "name",
		"publicAccessID":             "public_access_id",
		"publicAccessIDValue":        "public_access_id_value",
		"acronymPreservingCamelCase": "acronym_preserving_camel_case",
		"ID":                         "id",
		"IPAddress":                  "ip_address",
		"publicHTTPAPIURL":           "public_httpapiurl",
	} {
		t.Run(internal, func(t *testing.T) { require.Equal(t, public, snake(internal)) })
	}
}
