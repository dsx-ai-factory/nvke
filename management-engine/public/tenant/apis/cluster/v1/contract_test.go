// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Contract tests keep generated schemas and serialized messages consistent for API consumers.

package clusterv1_test

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"

	openapioptions "github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-openapiv2/options"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"sigs.k8s.io/yaml"

	clusterv1 "github.com/dsx-ai-factory/nvke/management-engine/public/tenant/gen/cluster/v1"
)

// TestJSONContractUsesOnlySnakeCase checks field names and rejects unsupported input.
// Clients must not gain accidental aliases or access to private fields.
func TestJSONContractUsesOnlySnakeCase(t *testing.T) {
	files := []protoreflect.FileDescriptor{
		clusterv1.File_cluster_v1_cluster_proto, clusterv1.File_cluster_v1_spec_proto,
	}
	for _, file := range files {
		messages := file.Messages()
		for i := 0; i < messages.Len(); i++ {
			fields := messages.Get(i).Fields()
			for j := 0; j < fields.Len(); j++ {
				field := fields.Get(j)
				require.Equal(t, string(field.Name()), field.JSONName(), "%s", field.FullName())
			}
		}
	}
	for _, input := range []string{
		`{"controlPlaneClass":"standard"}`,
		`{"authentication":{}}`,
		`{"release":{"channel":{"name":"one","name":"two"}}}`,
		`{"cluster_network":{"pods":{"cidrBlocks":["10.0.0.0/8"]}}}`,
	} {
		require.Error(t, protojson.Unmarshal([]byte(input), &clusterv1.CreateClusterSpec{}))
	}
	for _, input := range []string{
		`{"cluster_network":{}}`,
		`{"api_server_access":{}}`,
	} {
		require.Error(t, protojson.Unmarshal([]byte(input), &clusterv1.ReplaceClusterSpec{}))
	}
}

// TestOpenAPIContractMatchesPublicJSON compares routes and schemas with protobuf descriptors.
// The standalone handoff must describe the same API that generated clients use.
func TestOpenAPIContractMatchesPublicJSON(t *testing.T) {
	data := readOpenAPIContract(t)
	type parameter struct{ Name, In string }
	var document struct {
		Swagger string
		Info    struct {
			Title, Version, Description string
			License                     struct{ Name, URL string }
		}
		Paths       map[string]map[string]struct{ Parameters []parameter }
		Definitions map[string]struct {
			Type       string
			Enum       []string
			Properties map[string]struct {
				Type, Format string
				Ref          string `json:"$ref"`
			}
		}
	}
	require.NoError(t, json.Unmarshal(data, &document))
	require.Equal(t, "2.0", document.Swagger)
	require.Equal(t, "Tenant Cluster API", document.Info.Title)
	require.Equal(t, "v1", document.Info.Version)
	require.Equal(t, "Apache-2.0", document.Info.License.Name)
	require.Equal(t, "https://www.apache.org/licenses/LICENSE-2.0", document.Info.License.URL)
	require.Contains(t, document.Info.Description, "exactly one canonical IPv4")
	require.Contains(t, document.Info.Description, "must not overlap")
	routes := map[string][]parameter{}
	for path, methods := range document.Paths {
		for method, operation := range methods {
			routes[method+" "+path] = operation.Parameters
		}
	}
	wantRoutes := map[string][]parameter{
		"get /v1/clusters":        {{Name: "page_size", In: "query"}, {Name: "page_token", In: "query"}},
		"post /v1/clusters":       {{Name: "body", In: "body"}},
		"get /v1/clusters/{name}": {{Name: "name", In: "path"}},
		"put /v1/clusters/{name}": {{Name: "name", In: "path"}, {Name: "body", In: "body"}},
	}
	require.Len(t, routes, len(wantRoutes))
	for route, parameters := range wantRoutes {
		require.Contains(t, routes, route)
		require.ElementsMatch(t, parameters, routes[route], "%s", route)
	}
	for _, file := range []protoreflect.FileDescriptor{
		clusterv1.File_cluster_v1_cluster_proto, clusterv1.File_cluster_v1_spec_proto,
	} {
		messages := file.Messages()
		for i := range messages.Len() {
			message := messages.Get(i)
			definitionName, pathField := "v1"+string(message.Name()), protoreflect.Name("")
			switch message.Name() {
			case "GetClusterRequest", "ListClustersRequest":
				continue // These requests contain only path and query parameters.
			case "ReplaceClusterRequest":
				definitionName, pathField = "ClusterServiceReplaceClusterBody", "name"
			}
			definition, found := document.Definitions[definitionName]
			require.True(t, found, "%s", definitionName)
			fields, count := message.Fields(), 0
			for j := range fields.Len() {
				field := fields.Get(j)
				if field.Name() != pathField {
					require.Contains(t, definition.Properties, field.JSONName(), "%s", definitionName)
					count++
				}
			}
			require.Len(t, definition.Properties, count, "%s must expose exactly its protobuf fields", definitionName)
		}
	}
	for name, definition := range document.Definitions {
		if strings.HasPrefix(name, "v1") || name == "ClusterServiceReplaceClusterBody" {
			for field := range definition.Properties {
				require.Regexp(t, `^[a-z][a-z0-9]*(_[a-z0-9]+)*$`, field, "%s.%s", name, field)
			}
		}
	}
	int64Fields := map[string]string{"v1Cluster": "generation", "v1ClusterStatus": "observed_generation"}
	for definition, field := range int64Fields {
		property := document.Definitions[definition].Properties[field]
		require.Equal(t, "string", property.Type, "%s.%s", definition, field)
		require.Equal(t, "int64", property.Format, "%s.%s", definition, field)
	}
	statusFields := (&clusterv1.ClusterStatus{}).ProtoReflect().Descriptor().Fields()
	for _, field := range []protoreflect.Name{"state", "reason"} {
		property := document.Definitions["v1ClusterStatus"].Properties[string(field)]
		definition := document.Definitions[strings.TrimPrefix(property.Ref, "#/definitions/")]
		require.Equal(t, "string", definition.Type, "%s", field)
		values := statusFields.ByName(field).Enum().Values()
		var names []string
		for i := 0; i < values.Len(); i++ {
			if value := values.Get(i); value.Number() != 0 {
				names = append(names, string(value.Name()))
			}
		}
		require.Equal(t, names, definition.Enum, "%s", field)
	}
}

// readOpenAPIContract decodes the generated YAML into JSON for shared assertions.
// Tests inspect the delivered artifact rather than reconstructing its schema.
func readOpenAPIContract(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../../../gen/cluster/v1/openapi.yaml")
	require.NoError(t, err)
	data, err = yaml.YAMLToJSON(data)
	require.NoError(t, err)
	return data
}

// TestOpenAPIEnvelopeConstraints checks requiredness, bounds and operation-specific schemas.
// Consumers need accurate requirements even where the generator preserves only prose.
func TestOpenAPIEnvelopeConstraints(t *testing.T) {
	type schema struct {
		MinItems, MaxItems                 int
		Items                              *schema
		ReadOnly                           bool
		Type, Format, Description, Pattern string
		Required                           []string
		MinLength, MaxLength               int
		Minimum, Maximum                   *float64
		Properties                         map[string]json.RawMessage
	}
	type parameter struct {
		Name, In, Type, Description, Pattern string
		Required                             bool
		Default                              json.RawMessage
	}
	var document struct {
		Definitions map[string]schema
		Paths       map[string]map[string]struct {
			Summary, Description string
			Parameters           []parameter
		}
	}
	require.NoError(t, json.Unmarshal(readOpenAPIContract(t), &document))
	property := func(definition, field string) schema {
		t.Helper()
		require.Contains(t, document.Definitions, definition)
		require.Contains(t, document.Definitions[definition].Properties, field)
		var out schema
		require.NoError(t, json.Unmarshal(document.Definitions[definition].Properties[field], &out))
		return out
	}
	fieldOptions := func(message proto.Message, name protoreflect.Name) *openapioptions.JSONSchema {
		t.Helper()
		field := message.ProtoReflect().Descriptor().Fields().ByName(name)
		require.NotNil(t, field)
		require.True(t, proto.HasExtension(field.Options(), openapioptions.E_Openapiv2Field))
		options, ok := proto.GetExtension(field.Options(), openapioptions.E_Openapiv2Field).(*openapioptions.JSONSchema)
		require.True(t, ok)
		return options
	}
	require.ElementsMatch(t, []string{"name", "spec"}, document.Definitions["v1CreateClusterRequest"].Required)
	require.ElementsMatch(t, []string{"resource_version", "spec"}, document.Definitions["ClusterServiceReplaceClusterBody"].Required)
	require.NotContains(t, document.Definitions["ClusterServiceReplaceClusterBody"].Properties, "name")
	require.Equal(t, []string{"state"}, document.Definitions["v1ClusterStatus"].Required)
	require.NotContains(t, document.Definitions["v1ListClustersResponse"].Required, "clusters")
	for _, tc := range []struct {
		definition string
		required   []string
	}{
		{"v1CreateClusterSpec", []string{"release", "control_plane_class", "cluster_network"}},
		{"v1ReplaceClusterSpec", []string{"release", "control_plane_class"}},
	} {
		require.ElementsMatch(t, tc.required, document.Definitions[tc.definition].Required, tc.definition)
		for field := range document.Definitions[tc.definition].Properties {
			require.False(t, property(tc.definition, field).ReadOnly, "%s.%s is writable", tc.definition, field)
		}
	}
	require.NotContains(t, document.Definitions["v1ReplaceClusterSpec"].Properties, "cluster_network")
	for _, tc := range []struct {
		definition string
		maxItems   int
	}{
		{"v1CreateClusterNetworkPods", 1},
		{"v1CreateClusterNetworkServices", 1},
		{"v1ReadClusterNetworkPods", 2},
		{"v1ReadClusterNetworkServices", 2},
	} {
		cidrs := property(tc.definition, "cidr_blocks")
		require.Equal(t, "array", cidrs.Type, tc.definition)
		require.Equal(t, 1, cidrs.MinItems, tc.definition)
		require.Equal(t, tc.maxItems, cidrs.MaxItems, tc.definition)
		require.NotNil(t, cidrs.Items, tc.definition)
		require.Equal(t, "string", cidrs.Items.Type, tc.definition)
		require.Equal(t, 49, cidrs.Items.MaxLength, tc.definition)
	}
	for _, definition := range []string{"v1CreateReleaseChannel", "v1ReplaceReleaseChannel", "v1ReadReleaseChannel"} {
		version := property(definition, "version")
		require.Equal(t, 1, version.MinLength, definition)
		require.Equal(t, 128, version.MaxLength, definition)
		require.NotContains(t, document.Definitions[definition].Required, "version", definition)
	}
	for _, private := range []string{"api_server_access", "public_access_enabled", "service_domain", "cluster.local", "x-kubernetes-validations"} {
		require.NotContains(t, string(readOpenAPIContract(t)), private, "private defaults and raw admission rules are not public schemas")
	}
	const namePattern = `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	for _, definition := range []string{"v1Cluster", "v1CreateClusterRequest"} {
		name := property(definition, "name")
		require.Equal(t, 1, name.MinLength)
		require.Equal(t, 253, name.MaxLength)
		require.Equal(t, namePattern, name.Pattern)
	}
	version := property("ClusterServiceReplaceClusterBody", "resource_version")
	require.Equal(t, 1, version.MinLength)
	require.Equal(t, 1024, version.MaxLength)
	require.Contains(t, version.Description, "bytes")
	require.Empty(t, version.Pattern, "opaque versions must not be constrained to numeric values")
	for definition, field := range map[string]string{"v1Cluster": "generation", "v1ClusterStatus": "observed_generation"} {
		value := property(definition, field)
		require.Equal(t, "string", value.Type)
		require.Equal(t, "int64", value.Format)
		require.Contains(t, value.Description, "decimal string")
		require.Nil(t, value.Minimum, "%s.%s is a JSON string", definition, field)
		require.Nil(t, value.Maximum, "%s.%s is a JSON string", definition, field)
	}
	for _, method := range []string{"get", "put"} {
		operation := document.Paths["/v1/clusters/{name}"][method]
		require.NotEmpty(t, operation.Summary+operation.Description)
		var found bool
		for _, value := range operation.Parameters {
			if value.Name != "name" {
				continue
			}
			found = true
			require.Equal(t, "path", value.In)
			require.True(t, value.Required)
			require.Equal(t, namePattern, value.Pattern)
			require.Contains(t, value.Description, "1 to 253")
		}
		require.True(t, found, "%s needs the cluster name path parameter", method)
	}
	// The pinned plugin preserves parameter type, pattern and descriptions, but
	// omits numeric and length bounds from path/query parameters. Keep those
	// requirements in both field metadata and the standalone document's prose.
	for _, request := range []proto.Message{&clusterv1.GetClusterRequest{}, &clusterv1.ReplaceClusterRequest{}} {
		name := fieldOptions(request, "name")
		require.Equal(t, uint64(1), name.GetMinLength())
		require.Equal(t, uint64(253), name.GetMaxLength())
		require.Equal(t, namePattern, name.GetPattern())
	}
	listParameters := map[string]parameter{}
	for _, value := range document.Paths["/v1/clusters"]["get"].Parameters {
		listParameters[value.Name] = value
	}
	size := listParameters["page_size"]
	require.Equal(t, "integer", size.Type)
	for _, requirement := range []string{"1 to 100", "Zero or omission uses 50", "must match the token's page size"} {
		require.Contains(t, size.Description, requirement)
	}
	sizeOptions := fieldOptions(&clusterv1.ListClustersRequest{}, "page_size")
	require.Equal(t, float64(-1), sizeOptions.GetMinimum())
	require.True(t, sizeOptions.GetExclusiveMinimum())
	require.Equal(t, float64(100), sizeOptions.GetMaximum())
	require.False(t, size.Required)
	require.Empty(t, size.Default, "a fixed default would change continuation semantics")
	tokenOptions := fieldOptions(&clusterv1.ListClustersRequest{}, "page_token")
	require.Equal(t, uint64(16384), tokenOptions.GetMaxLength())
	require.Contains(t, listParameters["page_token"].Description, "16384 bytes")
	require.False(t, listParameters["page_token"].Required)
	for _, definition := range []string{
		"v1Cluster", "v1ClusterStatus", "v1CreateClusterRequest", "ClusterServiceReplaceClusterBody", "v1ListClustersResponse",
	} {
		require.NotEmpty(t, document.Definitions[definition].Description, "%s needs a description", definition)
		for field := range document.Definitions[definition].Properties {
			require.NotEmpty(t, property(definition, field).Description, "%s.%s needs a description", definition, field)
		}
	}
}

// TestStatusEnumJSONContract checks named, omitted and future enum values on the wire.
// Sparse reasons and unknown values must retain their documented serialization behavior.
func TestStatusEnumJSONContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status *clusterv1.ClusterStatus
		json   string
	}{
		{"pending", &clusterv1.ClusterStatus{
			State: clusterv1.ClusterStatus_STATE_PENDING, Reason: clusterv1.ClusterStatus_REASON_AWAITING_STATUS},
			`{"state":"STATE_PENDING","reason":"REASON_AWAITING_STATUS"}`},
		{"provisioning", &clusterv1.ClusterStatus{
			State: clusterv1.ClusterStatus_STATE_PENDING, Reason: clusterv1.ClusterStatus_REASON_PROVISIONING},
			`{"state":"STATE_PENDING","reason":"REASON_PROVISIONING"}`},
		{"ready has no redundant reason", &clusterv1.ClusterStatus{
			State: clusterv1.ClusterStatus_STATE_READY, ObservedGeneration: 2},
			`{"state":"STATE_READY","observed_generation":"2"}`},
		{"deleting has no redundant reason", &clusterv1.ClusterStatus{
			State: clusterv1.ClusterStatus_STATE_DELETING}, `{"state":"STATE_DELETING"}`},
		{"creation rejected", &clusterv1.ClusterStatus{
			State: clusterv1.ClusterStatus_STATE_FAILED, Reason: clusterv1.ClusterStatus_REASON_CREATION_REJECTED},
			`{"state":"STATE_FAILED","reason":"REASON_CREATION_REJECTED"}`},
		{"unsupported configuration", &clusterv1.ClusterStatus{
			State: clusterv1.ClusterStatus_STATE_FAILED, Reason: clusterv1.ClusterStatus_REASON_UNSUPPORTED_CONFIGURATION},
			`{"state":"STATE_FAILED","reason":"REASON_UNSUPPORTED_CONFIGURATION"}`},
		{"unsupported update", &clusterv1.ClusterStatus{
			State: clusterv1.ClusterStatus_STATE_FAILED, Reason: clusterv1.ClusterStatus_REASON_UNSUPPORTED_UPDATE},
			`{"state":"STATE_FAILED","reason":"REASON_UNSUPPORTED_UPDATE"}`},
		{"provisioning failed", &clusterv1.ClusterStatus{
			State: clusterv1.ClusterStatus_STATE_FAILED, Reason: clusterv1.ClusterStatus_REASON_PROVISIONING_FAILED},
			`{"state":"STATE_FAILED","reason":"REASON_PROVISIONING_FAILED"}`},
		{"deletion blocked", &clusterv1.ClusterStatus{
			State: clusterv1.ClusterStatus_STATE_DELETING, Reason: clusterv1.ClusterStatus_REASON_DELETION_BLOCKED},
			`{"state":"STATE_DELETING","reason":"REASON_DELETION_BLOCKED"}`},
		{"operational unknown", &clusterv1.ClusterStatus{
			State: clusterv1.ClusterStatus_STATE_UNKNOWN, Reason: clusterv1.ClusterStatus_REASON_STATUS_UNAVAILABLE},
			`{"state":"STATE_UNKNOWN","reason":"REASON_STATUS_UNAVAILABLE"}`},
		{"deletion status unavailable", &clusterv1.ClusterStatus{
			State: clusterv1.ClusterStatus_STATE_DELETING, Reason: clusterv1.ClusterStatus_REASON_STATUS_UNAVAILABLE},
			`{"state":"STATE_DELETING","reason":"REASON_STATUS_UNAVAILABLE"}`},
		{"unset is not operational unknown", &clusterv1.ClusterStatus{}, `{}`},
		{"unknown numeric value", &clusterv1.ClusterStatus{
			State: clusterv1.ClusterStatus_State(999), Reason: clusterv1.ClusterStatus_Reason(999)},
			`{"state":999,"reason":999}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := protojson.Marshal(tc.status)
			require.NoError(t, err)
			require.JSONEq(t, tc.json, string(encoded))
			var decoded clusterv1.ClusterStatus
			require.NoError(t, protojson.Unmarshal(encoded, &decoded))
			require.True(t, proto.Equal(tc.status, &decoded))
			if tc.status.State != clusterv1.ClusterStatus_State(999) {
				return
			}
			// Unknown numeric values must survive decoding by an older client.
			binary, err := proto.Marshal(tc.status)
			require.NoError(t, err)
			require.NoError(t, proto.Unmarshal(binary, &decoded))
			require.True(t, proto.Equal(tc.status, &decoded))
		})
	}
	for _, input := range []string{
		`{"state":"STATE_FUTURE"}`, `{"reason":"REASON_FUTURE"}`, `{"state":"pending"}`,
		`{"reason":"REASON_RECONCILED"}`, `{"reason":"REASON_DELETION_REQUESTED"}`,
	} {
		require.Error(t, protojson.Unmarshal([]byte(input), &clusterv1.ClusterStatus{}), input)
	}
	// Status is structurally absent from mutation requests, including numeric enums.
	for _, request := range []proto.Message{&clusterv1.CreateClusterRequest{}, &clusterv1.ReplaceClusterRequest{}} {
		for _, input := range []string{
			`{"status":{"state":"STATE_READY"}}`, `{"status":{"state":999}}`,
		} {
			require.Error(t, protojson.Unmarshal([]byte(input), request))
		}
		require.Nil(t, request.ProtoReflect().Descriptor().Fields().ByName("status"))
	}
}

// TestStatusOmissionAndGenerationJSON checks absent assessments and large generation values.
// Serialization must preserve missing information and integer precision.
func TestStatusOmissionAndGenerationJSON(t *testing.T) {
	var cluster clusterv1.Cluster
	require.NoError(t, protojson.Unmarshal([]byte(`{"name":"example"}`), &cluster))
	require.Nil(t, cluster.Status, "an omitted assessment must remain absent")
	var status clusterv1.ClusterStatus
	require.NoError(t, protojson.Unmarshal([]byte(`{"state":"STATE_READY","reason":"REASON_UNSPECIFIED"}`), &status))
	require.Equal(t, clusterv1.ClusterStatus_REASON_UNSPECIFIED, status.Reason)
	encoded, err := protojson.Marshal(&status)
	require.NoError(t, err)
	require.JSONEq(t, `{"state":"STATE_READY"}`, string(encoded))
	// MaxInt64 exceeds JavaScript's exact integer range and exercises the upper bound.
	cluster = clusterv1.Cluster{Generation: math.MaxInt64, Status: &clusterv1.ClusterStatus{
		State: clusterv1.ClusterStatus_STATE_READY, ObservedGeneration: math.MaxInt64,
	}}
	encoded, err = protojson.Marshal(&cluster)
	require.NoError(t, err)
	require.JSONEq(t, `{"generation":"9223372036854775807","status":{"state":"STATE_READY","observed_generation":"9223372036854775807"}}`, string(encoded))
}

// TestEnvelopeFieldNumbersAreStable checks the handwritten protobuf field assignments.
// An accidental renumbering must not change how existing binary messages are interpreted.
func TestEnvelopeFieldNumbersAreStable(t *testing.T) {
	for _, test := range []struct {
		message protoreflect.MessageDescriptor
		names   []protoreflect.Name
	}{
		{(&clusterv1.Cluster{}).ProtoReflect().Descriptor(),
			[]protoreflect.Name{"name", "resource_version", "spec", "status", "uid", "generation"}},
		{(&clusterv1.CreateClusterRequest{}).ProtoReflect().Descriptor(), []protoreflect.Name{"name", "spec"}},
		{(&clusterv1.GetClusterRequest{}).ProtoReflect().Descriptor(), []protoreflect.Name{"name"}},
		{(&clusterv1.ReplaceClusterRequest{}).ProtoReflect().Descriptor(),
			[]protoreflect.Name{"name", "resource_version", "spec"}},
		{(&clusterv1.ListClustersRequest{}).ProtoReflect().Descriptor(), []protoreflect.Name{"page_size", "page_token"}},
	} {
		for i, name := range test.names {
			field := test.message.Fields().ByName(name)
			require.NotNil(t, field)
			require.Equal(t, protoreflect.FieldNumber(i+1), field.Number())
		}
	}
}
