// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
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
	return loadSurfaceDocuments(t, []publicapi.Surface{publicapi.Tenant(), publicapi.Operator()})
}

func loadSurfaceDocuments(t *testing.T, definitions []publicapi.Surface) map[string]*openapi3.T {
	t.Helper()
	docs, err := documents("../..", definitions)
	require.NoError(t, err)
	for name, doc := range docs {
		data, err := encode(doc)
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
		"network is required when node_pools are configured", "authentication is only configurable at cluster creation",
	} {
		require.Contains(t, cluster.Description, description)
	}
	pools := cluster.Properties["node_pools"].Value
	require.Contains(t, pools.Description, "unique combinations of these keys: name.")
	taints := pools.Items.Value.Properties["user_params"].Value.Properties["taints"].Value
	require.Contains(t, taints.Description, "unique combinations of these keys: key, effect.")
	require.Equal(t, "map", taints.Extensions["x-kubernetes-list-type"])
	require.False(t, taints.UniqueItems, "map keys are not equivalent to whole-item uniqueness")
	jwt := cluster.Properties["authentication"].Value.Properties["jwt"].Value
	require.Contains(t, jwt.Description, "issuer URLs must be unique")
	audiences := jwt.Items.Value.Properties["issuer"].Value.Properties["audiences"].Value
	require.True(t, audiences.UniqueItems)
	require.Equal(t, "set", audiences.Extensions["x-kubernetes-list-type"])

	nodes := tenant.Paths.Value("/v1/node-profiles").Get
	require.Contains(t, parameter(t, nodes, "field_selector").Description, "spec.profile, spec.tag")
	require.Equal(t, uint64(resource.MaxFieldSelectorBytes), *parameter(t, nodes, "field_selector").Schema.Value.MaxLength)

	catalog := tenant.Components.Schemas["NodeProfileSpec"].Value
	require.Contains(t, catalog.Properties, "operating_system")
	require.Contains(t, catalog.Properties, "architecture")
	// Fields inlined from a projection keep their source's documentation and limits.
	require.NotEmpty(t, catalog.Properties["architecture"].Value.Description)
	require.NotEmpty(t, catalog.Properties["tag"].Value.Pattern)
	channel := tenant.Components.Schemas["ClusterReleaseChannelSpec"].Value
	require.NotEmpty(t, channel.Properties["versions"].Value.Description)
	require.Empty(t, channel.Properties["versions"].Value.Items.Value.Description)
	require.NotContains(t, catalog.Properties, "machine_image_ref")
	require.NotContains(t, catalog.Properties, "machine_init")
	require.NotContains(t, catalog.Properties["profile"].Value.Description, "profile is immutable")
	lifecycle := catalog.Properties["lifecycle"].Value
	require.NotContains(t, lifecycle.Properties, "hidden")
	require.Contains(t, lifecycle.Required, "deprecated")
	require.Equal(t, "date-time", lifecycle.Properties["supported_until"].Value.Format)
	class := tenant.Components.Schemas["ControlPlaneClassSpec"].Value
	require.NotContains(t, class.Properties, "rollout")
	require.NotContains(t, class.Description, "max_surge")
	replicas := class.Properties["control_plane"].Value.Properties["replicas"].Value
	require.Equal(t, float64(1), *replicas.Min)

	writeSpec := operator.Components.Schemas["ControlPlaneClassSpec"].Value
	require.Contains(t, writeSpec.Required, "rollout")
	require.Contains(t, writeSpec.Properties["control_plane"].Value.Required, "replicas")
	require.NotContains(t, writeSpec.Required, "lifecycle") // +optional overrides the non-omitempty JSON tag.
	require.Contains(t, writeSpec.Description, "single-replica control planes require max_surge 1")
	profile := operator.Components.Schemas["NodeProfileSpec"].Value.Properties["profile"].Value
	require.Contains(t, profile.Description, "profile is immutable")
	require.False(t, profile.ReadOnly, "immutable fields remain writable at creation")
	metadata := operator.Components.Schemas["MachineImage"].Value.Properties["metadata"].Value
	require.Equal(t, "date-time", metadata.Properties["creation_timestamp"].Value.Format)
	for _, field := range []string{"namespace", "resource_version", "managed_fields", "annotations"} {
		require.NotContains(t, metadata.Properties, field)
	}
}

// Generated prose must use published kinds, including descriptions outside schemas.
func TestDescriptionsUsePublishedKinds(t *testing.T) {
	published, err := kindNames([]publicapi.Surface{publicapi.Tenant(), publicapi.Operator()})
	require.NoError(t, err)
	var check func(*testing.T, any)
	check = func(t *testing.T, node any) {
		t.Helper()
		switch value := node.(type) {
		case map[string]any:
			for key, child := range value {
				if description, ok := child.(string); key == "description" && ok {
					require.Equal(t, published.Replace(description), description, "unpublished kind in description")
				}
				check(t, child)
			}
		case []any:
			for _, child := range value {
				check(t, child)
			}
		}
	}
	for name, doc := range loadDocuments(t) {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(doc)
			require.NoError(t, err)
			var value any
			require.NoError(t, json.Unmarshal(data, &value))
			check(t, value)
		})
	}
}

func TestDescriptionRewritingPreservesContract(t *testing.T) {
	published, err := kindNames([]publicapi.Surface{publicapi.Tenant(), publicapi.Operator()})
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
	require.NoError(t, nameSchemas(spec, resource.CamelCase, published))
	before, err := json.Marshal(spec)
	require.NoError(t, err)
	data, err := encode(spec)
	require.NoError(t, err)
	var decoded openapi3.T
	require.NoError(t, yaml.Unmarshal(data, &decoded))
	require.Equal(t, "Works with NVKECluster resources.", decoded.Info.Description,
		"handwritten overview prose is not schema documentation")
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
	require.Equal(t, "NVKECluster", decoded.Components.Examples["default"].Value.Description)
	require.Equal(t, literal, decoded.Components.Examples["default"].Value.Value)
	resultField := result.Properties["description"].Value
	require.Equal(t, "ClusterStatus from the NVKE controller.", resultField.Description)
	require.Equal(t, field.Pattern, resultField.Pattern)
	require.Equal(t, field.Enum, resultField.Enum)
	require.Equal(t, uint64(largeLimit), *resultField.MaxLength)
	after, err := json.Marshal(spec)
	require.NoError(t, err)
	require.Equal(t, before, after, "encoding must not mutate the document")
}

func TestKindNamesUseAllSurfaces(t *testing.T) {
	profile := func(kind string, verbs ...resource.Operation) resource.Registration {
		return resource.NewRegistration[v1.PublicNodeProfile, v1.NodeProfile]("/v1/node-profiles", resource.ResourceInfo{
			GVK: v1.SchemeGroupVersion.WithKind(kind), Verbs: verbs,
		})
	}
	definitions := []publicapi.Surface{
		{Name: "tenant", Registrations: []resource.Registration{publicapi.NVKECluster(resource.OpGet)}},
		{Name: "operator", Registrations: []resource.Registration{
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
	definitions[1].Registrations = append(definitions[1].Registrations, profile("NVKECluster", resource.OpGet))
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
	doc, err := document(publicapi.Surface{Name: "tenant", Registrations: registrations}, builder, strings.NewReplacer())
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
	}, "Widget", resource.SnakeCase)
	require.ErrorContains(t, err, `undocumented query parameter "newQuery"`)
}

func TestRequiredBoolean(t *testing.T) {
	doc := loadDocuments(t)["tenant"]
	schema := doc.Components.Schemas["ClusterSpec"].Value.Properties["api_server_access"].Value
	require.NoError(t, schema.VisitJSON(map[string]any{"public_access_enabled": false}, openapi3.VisitAsRequest()))
	require.Error(t, schema.VisitJSON(map[string]any{}, openapi3.VisitAsRequest()))
}

func TestIndependentSurfaceNaming(t *testing.T) {
	type namingCase struct {
		name, pageKey, pageSize, hasMore, fieldSelector, propagationPolicy, creationTimestamp string
		invalidRequest, invalidJSON, controlPlane, operatingSystem, nodePools, vpcID          string
		naming                                                                                resource.Naming
	}
	formats := []namingCase{
		{"snake", "page_key", "page_size", "has_more", "field_selector", "propagation_policy", "creation_timestamp",
			"invalid_request", "invalid_json", "control_plane", "operating_system", "node_pools", "vpc_id", resource.SnakeCase},
		{"camel", "pageKey", "pageSize", "hasMore", "fieldSelector", "propagationPolicy", "creationTimestamp",
			"invalidRequest", "invalidJson", "controlPlane", "operatingSystem", "nodePools", "vpcID", resource.CamelCase},
	}
	for _, tenant := range formats {
		for _, operator := range formats {
			t.Run(tenant.name+"/"+operator.name, func(t *testing.T) {
				tenantAPI, operatorAPI := publicapi.Tenant(), publicapi.Operator()
				tenantAPI.Naming, operatorAPI.Naming = tenant.naming, operator.naming
				docs := loadSurfaceDocuments(t, []publicapi.Surface{tenantAPI, operatorAPI})
				for _, selected := range []struct {
					name   string
					format namingCase
				}{{"tenant", tenant}, {"operator", operator}} {
					format := selected.format
					doc := docs[selected.name]
					class := doc.Components.Schemas["ControlPlaneClassSpec"].Value
					require.Contains(t, class.Properties, format.controlPlane)
					require.Contains(t, class.Required, format.controlPlane)
					metadataResource := map[string]string{"tenant": "Cluster", "operator": "MachineImage"}[selected.name]
					metadata := doc.Components.Schemas[metadataResource].Value.Properties["metadata"].Value
					require.Contains(t, metadata.Properties, format.creationTimestamp)
					list := doc.Components.Schemas["ControlPlaneClassList"].Value
					require.Contains(t, list.Properties, format.pageKey)
					require.Contains(t, list.Required, format.hasMore)
					op := doc.Paths.Value("/v1/control-plane-classes").Get
					parameter(t, op, format.pageSize)
					require.Contains(t, parameter(t, op, format.pageKey).Description, format.pageKey)
					require.Contains(t, op.Description, format.hasMore)
					problem := doc.Components.Schemas["Problem"].Value
					require.Contains(t, problem.Properties["code"].Value.Enum, format.invalidRequest)
					fieldCodes := problem.Properties["errors"].Value.Items.Value.Properties["code"].Value.Enum
					require.Contains(t, fieldCodes, format.invalidJSON)
					require.NoError(t, doc.WalkSchemas(func(_ string, ref *openapi3.SchemaRef) error {
						require.NotContains(t, ref.Value.Extensions, "x-kubernetes-validations")
						return nil
					}))
				}
				cluster := docs["tenant"].Components.Schemas["ClusterSpec"].Value
				require.Contains(t, cluster.Properties, tenant.nodePools)
				require.Contains(t, cluster.Properties["network"].Value.Required, tenant.vpcID)
				catalog := docs["tenant"].Components.Schemas["NodeProfileSpec"].Value
				require.Contains(t, catalog.Properties, tenant.operatingSystem)
				parameter(t, docs["tenant"].Paths.Value("/v1/node-profiles").Get, tenant.fieldSelector)
				deleteClass := docs["operator"].Paths.Value("/v1/control-plane-classes/{name}").Delete
				parameter(t, deleteClass, operator.propagationPolicy)
			})
		}
	}
}

func TestSelectorNames(t *testing.T) {
	for _, test := range []struct {
		naming       resource.Naming
		query, field string
	}{
		{resource.SnakeCase, "field_selector", "spec.network.vpc_id"},
		{resource.CamelCase, "fieldSelector", "spec.network.vpcID"},
	} {
		op, err := operation(resource.Description{
			PublicType: reflect.TypeFor[v1.Cluster](), Fields: []string{"spec.network.vpcID"},
		}, resource.Route{Operation: resource.OpList, Query: []string{"fieldSelector"}}, "Cluster", test.naming)
		require.NoError(t, err)
		require.Contains(t, parameter(t, op, test.query).Description, test.field)
	}
}

func TestSchemaNamingPreservesData(t *testing.T) {
	const largeLimit = 1<<53 + 1
	value := openapi3.NewStringSchema().WithPattern("^nodeIDs$").WithEnum("nodeIDs").WithMaxLength(largeLimit)
	value.Default, value.Example = "nodeIDs", "nodeIDs"
	item := openapi3.NewObjectSchema().WithProperty("nodeIDs", value).WithRequired([]string{"nodeIDs"})
	item.Description = "nodeIDs must be present; nodeIDsSuffix is unrelated."
	list := openapi3.NewArraySchema().WithItems(item)
	list.Extensions = map[string]any{"x-kubernetes-list-map-keys": []string{"nodeIDs"}}
	dictionary := openapi3.NewObjectSchema().WithAdditionalProperties(item)
	dictionary.Example = map[string]any{"userKey": map[string]any{"node_ids": "nodeIDs"}}
	schema := openapi3.NewObjectSchema().WithProperty("nodeList", list).WithProperty("nodeMap", dictionary)
	spec := &openapi3.T{Components: &openapi3.Components{Schemas: openapi3.Schemas{"NodeIDs": {Value: schema}}}}
	require.NoError(t, nameSchemas(spec, resource.SnakeCase, strings.NewReplacer()))
	require.Contains(t, schema.Properties, "node_list")
	require.Contains(t, schema.Properties, "node_map")
	require.Equal(t, []string{"node_ids"}, list.Extensions["x-kubernetes-list-map-keys"])
	require.Equal(t, []string{"node_ids"}, item.Required)
	require.Contains(t, item.Properties, "node_ids")
	require.Equal(t, "node_ids must be present; nodeIDsSuffix is unrelated.", item.Description)
	require.Equal(t, "^nodeIDs$", value.Pattern)
	require.Equal(t, []any{"nodeIDs"}, value.Enum)
	require.Equal(t, "nodeIDs", value.Default)
	require.Equal(t, "nodeIDs", value.Example)
	require.Equal(t, uint64(largeLimit), *value.MaxLength)
	require.Equal(t, map[string]any{"userKey": map[string]any{"node_ids": "nodeIDs"}}, dictionary.Example)
	require.Contains(t, spec.Components.Schemas, "NodeIDs")

	collision := openapi3.NewObjectSchema().WithProperty("nodeIDs", value).WithProperty("node_ids", value)
	spec.Components.Schemas["Collision"] = &openapi3.SchemaRef{Value: collision}
	require.ErrorContains(t, nameSchemas(spec, resource.SnakeCase, strings.NewReplacer()), "collides")
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
