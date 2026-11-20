// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/dsx-ai-factory/nvke/management-engine/pkg/projection"
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"
)

var descriptionIdentifier = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// nameSchemas documents constraints and applies surface naming after canonical schema projection.
// Only declared properties are renamed: map keys, enum values, patterns, and literal defaults are data.
func nameSchemas(spec *openapi3.T, naming resource.Naming, published *strings.Replacer) error {
	fields := map[string]string{}
	if err := spec.WalkSchemas(func(_ string, ref *openapi3.SchemaRef) error {
		for name := range ref.Value.Properties {
			fields[name] = naming.FieldName(name)
		}
		return nil
	}); err != nil {
		return err
	}
	return spec.WalkSchemas(func(path string, ref *openapi3.SchemaRef) error {
		if err := describeConstraints(path, ref); err != nil {
			return err
		}
		schema := ref.Value
		if schema.Properties != nil {
			properties := openapi3.Schemas{}
			for _, name := range slices.Sorted(maps.Keys(schema.Properties)) {
				wireName := naming.FieldName(name)
				if _, exists := properties[wireName]; exists {
					return fmt.Errorf("%s: property %q collides as %q", path, name, wireName)
				}
				properties[wireName] = schema.Properties[name]
			}
			schema.Properties = properties
		}
		for i, name := range schema.Required {
			schema.Required[i] = naming.FieldName(name)
		}
		const listMapKeys = "x-kubernetes-list-map-keys"
		if value, exists := schema.Extensions[listMapKeys]; exists {
			keys, err := projection.JSON[[]string](value)
			if err != nil {
				return fmt.Errorf("%s: list-map keys: %w", path, err)
			}
			for i, key := range keys {
				keys[i] = naming.FieldName(key)
			}
			schema.Extensions[listMapKeys] = keys
		}
		// CEL messages have already become prose. Replace complete field identifiers only;
		// rules are omitted because CEL uses the canonical datastore field names.
		schema.Description = descriptionIdentifier.ReplaceAllStringFunc(schema.Description, func(word string) string {
			if name, exists := fields[word]; exists {
				return name
			}
			return word
		})
		schema.Description = published.Replace(schema.Description)
		return nil
	})
}
