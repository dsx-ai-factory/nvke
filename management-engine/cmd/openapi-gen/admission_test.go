// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/require"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	apivalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	v1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/publicapi"
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"
)

func TestSpecNullabilityMatchesAdmission(t *testing.T) {
	data, err := os.ReadFile("../../config/crd/bases/service.nvke.nvidia.com_controlplaneclasses.yaml")
	require.NoError(t, err)
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(data, &crd))
	source := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	var schema apiextensions.JSONSchemaProps
	require.NoError(t, apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&source, &schema, nil))
	structural, err := structuralschema.NewStructural(&schema)
	require.NoError(t, err)
	validator, _, err := apivalidation.NewSchemaValidator(&schema)
	require.NoError(t, err)
	sample, err := os.ReadFile("../../config/samples/service_v1_controlplaneclass.yaml")
	require.NoError(t, err)
	for _, naming := range []resource.Naming{resource.SnakeCase, resource.CamelCase} {
		surface := publicapi.Operator()
		surface.Naming = naming
		doc := loadSurfaceDocuments(t, []publicapi.Surface{surface})[surface.Name]
		for _, paths := range []struct{ canonical, snake []string }{
			{[]string{"lifecycle"}, []string{"lifecycle"}},
			{[]string{"lifecycle", "deprecated"}, []string{"lifecycle", "deprecated"}},
			{[]string{"lifecycle", "supportedUntil"}, []string{"lifecycle", "supported_until"}},
			{[]string{"controlPlane", "replicas"}, []string{"control_plane", "replicas"}},
			{[]string{"rollout", "strategy", "type"}, []string{"rollout", "strategy", "type"}},
		} {
			t.Run(strings.Join(paths.canonical, "."), func(t *testing.T) {
				var object map[string]any
				require.NoError(t, yaml.Unmarshal(sample, &object))
				spec := object["spec"].(map[string]any)
				require.NoError(t, unstructured.SetNestedField(spec, nil, paths.canonical...))
				wire := spec
				if naming == resource.SnakeCase {
					wire = map[string]any{
						"control_plane": map[string]any{
							"replicas": 3, "instance_type_preferences": []any{"1c3e5a7f-9b1d-4f3e-a5c7-e9b1d3f5a7c9"},
							"system_reserved_memory": "16Gi",
						},
						"rollout": map[string]any{"strategy": map[string]any{
							"type": "RollingUpdate", "rolling_update": map[string]any{"max_surge": 1},
						}},
					}
					require.NoError(t, unstructured.SetNestedField(wire, nil, paths.snake...))
				}
				schemaErr := doc.Components.Schemas["ControlPlaneClassSpec"].Value.VisitJSON(wire, openapi3.VisitAsRequest())
				defaulting.PruneNonNullableNullsWithoutDefaults(spec, structural)
				defaulting.Default(spec, structural)
				errors := apivalidation.ValidateCustomResource(nil, spec, validator)
				require.Equal(t, len(errors) == 0, schemaErr == nil, "admission %v, OpenAPI %v", errors, schemaErr)
			})
		}
	}
}

func TestCatalogVersionConstraints(t *testing.T) {
	doc := loadDocuments(t)["tenant"]
	schema := parameter(t, doc.Paths.Value("/v1/cluster-release-channels").Get, "version").Schema.Value
	for _, selector := range []string{
		"1.35", "1.35.0", "1.35.2-custom", "1.35.2.9999999999999999999999", "1.35.2+sha.abc",
		"1.35.2_" + strings.Repeat("a", 121), "1.35.2_" + strings.Repeat("a", 122),
		"", "1", "v1.35", "1.035.2", "1.35_custom", "1.35.2-", "1.35.2/a",
	} {
		_, err := (v1.ClusterReleaseChannel{}).FilterCatalogVersion(selector)
		schemaErr := schema.VisitJSON(selector)
		require.Equal(t, err == nil, schemaErr == nil, "%q: API %v, OpenAPI %v", selector, err, schemaErr)
	}
}
