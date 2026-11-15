// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	v1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/publicapi"
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"
)

func loadDocuments(t *testing.T) map[string]*openapi3.T {
	t.Helper()
	docs, err := documents("../..")
	require.NoError(t, err)
	published, err := kindNames(surfaces)
	require.NoError(t, err)
	for name, doc := range docs {
		data, err := encode(doc, published)
		require.NoError(t, err)
		docs[name], err = openapi3.NewLoader().LoadFromData(data)
		require.NoError(t, err)
		require.NoError(t, docs[name].Validate(t.Context()))
	}
	return docs
}

func TestPublicSchemas(t *testing.T) {
	docs := loadDocuments(t)
	for surface, doc := range docs {
		require.NoError(t, doc.WalkSchemas(func(path string, ref *openapi3.SchemaRef) error {
			require.NotContains(t, ref.Value.Extensions, "x-kubernetes-validations", "%s%s", surface, path)
			return nil
		}))
		name := doc.Components.Schemas["ResourceName"].Value
		require.Equal(t, "Immutable client-selected DNS-subdomain name.", name.Description)
		require.Nil(t, name.Default)
		require.False(t, name.ReadOnly)
	}
	tenant, operator := docs["tenant"], docs["operator"]
	require.Equal(t, "createCluster", tenant.Paths.Value("/v1/clusters").Post.OperationID)
	cluster := tenant.Components.Schemas["ClusterSpec"].Value
	for _, description := range []string{
		"ClusterSpec configures a cluster's control plane and node pools.",
		"network is required when nodePools are configured", "authentication is only configurable at cluster creation",
	} {
		require.Contains(t, cluster.Description, description)
	}
	pools := cluster.Properties["nodePools"].Value
	require.Contains(t, pools.Description, "unique combinations of these keys: name.")
	taints := pools.Items.Value.Properties["userParams"].Value.Properties["taints"].Value
	require.Contains(t, taints.Description, "unique combinations of these keys: key, effect.")
	require.Equal(t, "map", taints.Extensions["x-kubernetes-list-type"])
	require.False(t, taints.UniqueItems, "map keys are not equivalent to whole-item uniqueness")
	jwt := cluster.Properties["authentication"].Value.Properties["jwt"].Value
	require.Contains(t, jwt.Description, "issuer URLs must be unique")
	audiences := jwt.Items.Value.Properties["issuer"].Value.Properties["audiences"].Value
	require.True(t, audiences.UniqueItems)
	require.Equal(t, "set", audiences.Extensions["x-kubernetes-list-type"])

	nodes := tenant.Paths.Value("/v1/node-profiles").Get
	require.Contains(t, parameter(t, nodes, "fieldSelector").Description, "spec.profile, spec.tag")
	require.Equal(t, uint64(resource.MaxFieldSelectorBytes), *parameter(t, nodes, "fieldSelector").Schema.Value.MaxLength)

	catalog := tenant.Components.Schemas["NodeProfileSpec"].Value
	require.Contains(t, catalog.Properties, "operatingSystem")
	require.Contains(t, catalog.Properties, "architecture")
	// Fields inlined from a projection keep their source's documentation and limits.
	require.NotEmpty(t, catalog.Properties["architecture"].Value.Description)
	require.NotEmpty(t, catalog.Properties["tag"].Value.Pattern)
	channel := tenant.Components.Schemas["ClusterReleaseChannelSpec"].Value
	require.NotEmpty(t, channel.Properties["versions"].Value.Description)
	require.Empty(t, channel.Properties["versions"].Value.Items.Value.Description)
	require.NotContains(t, catalog.Properties, "machineImageRef")
	require.NotContains(t, catalog.Properties, "machineInit")
	require.NotContains(t, catalog.Properties["profile"].Value.Description, "profile is immutable")
	lifecycle := catalog.Properties["lifecycle"].Value
	require.NotContains(t, lifecycle.Properties, "hidden")
	require.Contains(t, lifecycle.Required, "deprecated")
	require.Equal(t, "date-time", lifecycle.Properties["supportedUntil"].Value.Format)
	class := tenant.Components.Schemas["ControlPlaneClassSpec"].Value
	require.NotContains(t, class.Properties, "rollout")
	require.NotContains(t, class.Description, "maxSurge")
	replicas := class.Properties["controlPlane"].Value.Properties["replicas"].Value
	require.Equal(t, float64(1), *replicas.Min)

	writeSpec := operator.Components.Schemas["ControlPlaneClassSpec"].Value
	require.Contains(t, writeSpec.Required, "rollout")
	require.Contains(t, writeSpec.Properties["controlPlane"].Value.Required, "replicas")
	require.NotContains(t, writeSpec.Required, "lifecycle") // +optional overrides the non-omitempty JSON tag.
	require.Contains(t, writeSpec.Description, "single-replica control planes require maxSurge 1")
	profile := operator.Components.Schemas["NodeProfileSpec"].Value.Properties["profile"].Value
	require.Contains(t, profile.Description, "profile is immutable")
	require.False(t, profile.ReadOnly, "immutable fields remain writable at creation")
	metadata := operator.Components.Schemas["MachineImage"].Value.Properties["metadata"].Value
	require.Equal(t, "date-time", metadata.Properties["creationTimestamp"].Value.Format)
	for _, field := range []string{"namespace", "resourceVersion", "managedFields", "annotations"} {
		require.NotContains(t, metadata.Properties, field)
	}
}

func TestDescriptionRewritingPreservesContract(t *testing.T) {
	published, err := kindNames(surfaces)
	require.NoError(t, err)
	const largeLimit = 1<<53 + 1
	field := openapi3.NewStringSchema().WithPattern("^NVKECluster$").WithMaxLength(largeLimit).
		WithEnum("NVKECluster", map[string]any{"description": "NVKECluster"})
	field.Description = "NVKEClusterStatus from the NVKE controller."
	schema := openapi3.NewObjectSchema().WithProperty("description", field).
		WithProperty("enum", openapi3.NewStringSchema())
	schema.Description = "NVKEClusterSpec"
	schema.Properties["enum"].Value.Description = "NVKECluster"
	literal := map[string]any{
		"description": "NVKECluster", "nested": []any{map[string]any{"description": "NVKEClusterSpec"}},
	}
	schema.Default, schema.Example = literal, literal
	schema.Extensions = map[string]any{"x-example": literal}
	for _, name := range []string{"default", "example", "examples", "value", "x-field"} {
		property := openapi3.NewStringSchema()
		property.Description = "NVKECluster"
		schema.WithProperty(name, property)
	}
	spec := &openapi3.T{
		OpenAPI: "3.0.3", Paths: openapi3.NewPaths(),
		Info: &openapi3.Info{Title: "Operator API", Version: "v1", Description: "Works with NVKECluster resources."},
		Components: &openapi3.Components{
			Schemas: openapi3.Schemas{"NVKECluster": {Value: schema}, "default": {Value: schema}},
			Examples: openapi3.Examples{"default": {Value: &openapi3.Example{
				Description: "NVKECluster", Value: literal,
			}}},
		},
	}
	data, err := encode(spec, published)
	require.NoError(t, err)
	var decoded openapi3.T
	require.NoError(t, yaml.Unmarshal(data, &decoded))
	require.Equal(t, "Works with Cluster resources.", decoded.Info.Description)
	require.Contains(t, decoded.Components.Schemas, "NVKECluster", "schema identifiers must not be rewritten")
	result := decoded.Components.Schemas["NVKECluster"].Value
	require.Equal(t, "ClusterSpec", result.Description)
	require.Equal(t, "Cluster", result.Properties["enum"].Value.Description)
	for _, name := range []string{"default", "example", "examples", "value", "x-field"} {
		require.Equal(t, "Cluster", result.Properties[name].Value.Description, name)
	}
	require.Equal(t, literal, result.Default)
	require.Equal(t, literal, result.Example)
	require.Equal(t, schema.Extensions, result.Extensions)
	require.Equal(t, "ClusterSpec", decoded.Components.Schemas["default"].Value.Description)
	require.Equal(t, "Cluster", decoded.Components.Examples["default"].Value.Description)
	require.Equal(t, literal, decoded.Components.Examples["default"].Value.Value)
	resultField := result.Properties["description"].Value
	require.Equal(t, "ClusterStatus from the NVKE controller.", resultField.Description)
	require.Equal(t, field.Pattern, resultField.Pattern)
	require.Equal(t, field.Enum, resultField.Enum)
	require.Equal(t, uint64(largeLimit), *resultField.MaxLength)
	require.Equal(t, "NVKEClusterSpec", schema.Description, "encoding must not mutate source schemas")
}

func TestKindNamesUseAllSurfaces(t *testing.T) {
	profile := func(kind string, verbs ...resource.Operation) resource.Registration {
		return resource.NewRegistration[v1.PublicNodeProfile, v1.NodeProfile]("/v1/node-profiles", resource.ResourceInfo{
			GVK: v1.SchemeGroupVersion.WithKind(kind), Verbs: verbs,
		})
	}
	definitions := []surface{
		{name: "tenant", registrations: []resource.Registration{publicapi.NVKECluster(resource.OpGet)}},
		{name: "operator", registrations: []resource.Registration{
			profile("NVKEClusterProfile", resource.OpGet), profile("NVKECluster"),
			resource.NewReadRegistration[v1.ControlPlaneClass]("/v1/cluster-views", resource.ResourceInfo{
				GVK: v1.SchemeGroupVersion.WithKind("NVKEClusterView"), Verbs: []resource.Operation{resource.OpGet},
			}),
		}},
	}
	published, err := kindNames(definitions)
	require.NoError(t, err)
	require.Equal(t, "ClusterSpec, NodeProfileSpec and NVKEClusterViewSpec from the NVKE controller.",
		published.Replace("NVKEClusterSpec, NVKEClusterProfileSpec and NVKEClusterViewSpec from the NVKE controller."))
	definitions[1].registrations = append(definitions[1].registrations, profile("NVKECluster", resource.OpGet))
	_, err = kindNames(definitions)
	require.ErrorContains(t, err, "kind NVKECluster is published as both Cluster and NodeProfile")
}

func parameter(t *testing.T, op *openapi3.Operation, name string) *openapi3.Parameter {
	t.Helper()
	parameter := op.Parameters.GetByInAndName(openapi3.ParameterInQuery, name)
	require.NotNil(t, parameter, "missing query parameter %s", name)
	return parameter
}

func TestDisabledRegistrationPublishesNoResourceSchemas(t *testing.T) {
	registrations := []resource.Registration{publicapi.NVKECluster()}
	builder, err := newSchemaBuilder(t.TempDir(), registrations)
	require.NoError(t, err)
	doc, err := document("tenant", registrations, builder)
	require.NoError(t, err)
	// A surface without resources still serves its health probes.
	require.Equal(t, 2, doc.Paths.Len())
	require.Nil(t, doc.Paths.Value("/v1/clusters"))
	require.Len(t, doc.Components.Schemas, 1)
	require.Contains(t, doc.Components.Schemas, "Problem")
}

func TestUndocumentedQueryFailsGeneration(t *testing.T) {
	_, err := operation(resource.Description{}, resource.Route{
		Operation: resource.OpList, Method: http.MethodGet, Path: "/widgets", Query: []string{"newQuery"},
	}, "Widget")
	require.ErrorContains(t, err, `undocumented query parameter "newQuery"`)
}

func TestRequiredBoolean(t *testing.T) {
	doc := loadDocuments(t)[tenantSurface]
	schema := doc.Components.Schemas["ClusterSpec"].Value.Properties["apiServerAccess"].Value
	require.NoError(t, schema.VisitJSON(map[string]any{"publicAccessEnabled": false}, openapi3.VisitAsRequest()))
	require.Error(t, schema.VisitJSON(map[string]any{}, openapi3.VisitAsRequest()))
}

func TestGenerationDetectsDrift(t *testing.T) {
	root := t.TempDir()
	crds := filepath.Join(root, "config/crd/bases")
	require.NoError(t, os.CopyFS(crds, os.DirFS("../../config/crd/bases")))
	require.NoError(t, generate(root, false))
	require.NoError(t, generate(root, true))
	file := filepath.Join(crds, "service.nvke.nvidia.com_controlplaneclasses.yaml")
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	for _, change := range []struct{ before, after string }{
		{"minimum: 1", "minimum: 2"},
		{"message: single-replica control planes require maxSurge 1", "message: A public constraint changed"},
	} {
		require.Contains(t, string(data), change.before)
		require.NoError(t, os.WriteFile(file, []byte(strings.ReplaceAll(string(data), change.before, change.after)), 0o644))
		require.ErrorContains(t, generate(root, true), "is stale")
		require.NoError(t, generate(root, false))
		require.NoError(t, generate(root, true))
	}
	require.NoError(t, os.Remove(filepath.Join(root, "api/operator.openapi.yaml")))
	require.ErrorContains(t, generate(root, true), "operator.openapi.yaml is stale")
	missingMessage := strings.ReplaceAll(string(data),
		"message: single-replica control planes require maxSurge 1", "message: ''")
	require.NoError(t, os.WriteFile(file, []byte(missingMessage), 0o644))
	require.ErrorContains(t, generate(root, false), "needs a message for public documentation")
}
