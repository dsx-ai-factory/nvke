// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nvke/controller/api"
)

func TestPublicOpenAPIStripsIgnoredSurface(t *testing.T) {
	t.Parallel()

	src := []byte(`
openapi: 3.0.3
servers:
  - url: /api
    x-fern-ignore: true
  - url: https://nvke.acme.example/api
paths:
  /hidden:
    get:
      x-fern-ignore: true
      operationId: hidden
  /visible:
    get:
      operationId: visible
      parameters:
        - name: keep
          in: query
          schema:
            type: string
        - name: drop
          in: query
          x-fern-ignore: true
          schema:
            type: string
components:
  schemas:
    Thing:
      type: object
      properties:
        name:
          type: string
        teleportEnabled:
          x-fern-ignore: true
          type: boolean
    Hidden:
      x-fern-ignore: true
      type: object
`)

	got, err := publicOpenAPI(src)
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(got, &doc))

	servers := doc["servers"].([]any)
	require.Len(t, servers, 2)
	assert.Equal(t, "/api", servers[0].(map[string]any)["url"])

	paths := doc["paths"].(map[string]any)
	assert.NotContains(t, paths, "/hidden")
	visible := paths["/visible"].(map[string]any)["get"].(map[string]any)
	params := visible["parameters"].([]any)
	require.Len(t, params, 1)
	assert.Equal(t, "keep", params[0].(map[string]any)["name"])

	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	assert.NotContains(t, schemas, "Hidden")
	props := schemas["Thing"].(map[string]any)["properties"].(map[string]any)
	assert.Contains(t, props, "name")
	assert.NotContains(t, props, "teleportEnabled")
}

func TestPublicOpenAPIHidesTeleportFromTheClusterSpec(t *testing.T) {
	t.Parallel()

	got, err := publicOpenAPI([]byte(api.OpenapiYAML))
	require.NoError(t, err)

	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(got)
	require.NoError(t, err)
	require.NoError(t, doc.Validate(context.Background()))

	assert.NotContains(t, strings.ToLower(string(got)), "teleport")
	assert.NotContains(t, doc.Paths.Map(), "/osProfiles")
	assert.NotContains(t, doc.Paths.Map(), "/configProfiles")

	cluster := doc.Components.Schemas["Cluster"].Value
	assert.NotContains(t, cluster.Properties, "teleportEnabled")
	assert.NotContains(t, cluster.Properties, "infra")
	assert.Contains(t, cluster.Properties, "name")

	require.NotEmpty(t, doc.Servers)
	assert.Equal(t, "/api", doc.Servers[0].URL)

	full := api.OpenapiYAML
	assert.Contains(t, full, "teleportEnabled")
}

func TestPublicOpenAPIRequiredNamesAreProperties(t *testing.T) {
	t.Parallel()

	got, err := publicOpenAPI([]byte(api.OpenapiYAML))
	require.NoError(t, err)

	var doc yaml.Node
	require.NoError(t, yaml.Unmarshal(got, &doc))

	var missing []string
	collectRequiredWithoutProperty(&doc, "", &missing)
	assert.Empty(t, missing)
}

func TestPublicOpenAPIAcceptsYAMLBooleanSpellings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		flag  string
		strip bool
	}{
		{name: "true", flag: "true", strip: true},
		{name: "True", flag: "True", strip: true},
		{name: "TRUE", flag: "TRUE", strip: true},
		{name: "false", flag: "false", strip: false},
		{name: "quoted true", flag: `"true"`, strip: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := []byte("properties:\n  keep:\n    type: string\n  drop:\n    x-fern-ignore: " + tt.flag + "\n    type: boolean\n")
			got, err := publicOpenAPI(src)
			require.NoError(t, err)

			var doc map[string]any
			require.NoError(t, yaml.Unmarshal(got, &doc))
			props := doc["properties"].(map[string]any)
			assert.Contains(t, props, "keep")
			if tt.strip {
				assert.NotContains(t, props, "drop")
				return
			}
			assert.Contains(t, props, "drop")
		})
	}
}

// collectRequiredWithoutProperty records required names that are not keys of
// the same mapping's properties. A required list with no properties mapping,
// such as a composition constraint, is not a schema object and is skipped.
func collectRequiredWithoutProperty(node *yaml.Node, path string, missing *[]string) {
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			collectRequiredWithoutProperty(child, path, missing)
		}
	case yaml.SequenceNode:
		for i, child := range node.Content {
			collectRequiredWithoutProperty(child, path+"["+strconv.Itoa(i)+"]", missing)
		}
	case yaml.MappingNode:
		props := mappingValue(node, "properties")
		required := mappingValue(node, "required")
		if props != nil && props.Kind == yaml.MappingNode && required != nil && required.Kind == yaml.SequenceNode {
			names := mappingKeys(props)
			where := path
			if where == "" {
				where = "required"
			}
			for _, item := range required.Content {
				if item.Kind != yaml.ScalarNode {
					continue
				}
				if _, ok := names[item.Value]; !ok {
					*missing = append(*missing, where+": "+item.Value)
				}
			}
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, val := node.Content[i], node.Content[i+1]
			collectRequiredWithoutProperty(val, joinYAMLPath(path, key.Value), missing)
		}
	case yaml.ScalarNode, yaml.AliasNode:
		return
	default:
		return
	}
}

func mappingValue(node *yaml.Node, name string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == name {
			return node.Content[i+1]
		}
	}
	return nil
}

func mappingKeys(node *yaml.Node) map[string]struct{} {
	names := make(map[string]struct{}, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		names[node.Content[i].Value] = struct{}{}
	}
	return names
}

func joinYAMLPath(parent, name string) string {
	if parent == "" {
		return name
	}
	if name == "" {
		return parent
	}
	return parent + "." + name
}
