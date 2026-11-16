// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/management-engine/pkg/publicapi"
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"
)

// publicName shares the naming rule used by schemas, operations and descriptions.
func publicName(description resource.Description) string {
	if description.ReadOnly {
		return description.Info.GVK.Kind
	}
	return strings.TrimPrefix(description.PublicType.Name(), "Public")
}

// kindNames includes every surface because descriptions can mention resources published elsewhere.
func kindNames(surfaces []publicapi.Surface) (*strings.Replacer, error) {
	published := map[string]string{}
	for _, surface := range surfaces {
		for _, registration := range surface.Registrations {
			description := registration.Describe()
			if len(description.Routes) == 0 {
				continue
			}
			kind, name := description.Info.GVK.Kind, publicName(description)
			if previous, found := published[kind]; found && previous != name {
				return nil, fmt.Errorf("kind %s is published as both %s and %s", kind, previous, name)
			}
			published[kind] = name
		}
	}
	// Longer names, including unchanged ones, shield their prefixes from replacement.
	kinds := slices.SortedFunc(maps.Keys(published), func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), cmp.Compare(a, b))
	})
	var pairs []string
	for _, kind := range kinds {
		pairs = append(pairs, kind, published[kind])
	}
	return strings.NewReplacer(pairs...), nil
}

// encode emits YAML with stable section order without changing the document.
func encode(spec *openapi3.T) ([]byte, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	// Preserve integer limits that cannot be represented exactly by float64.
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	// Lead with the overview and routes, then definitions and any remaining OpenAPI fields.
	sections := append([]string{"openapi", "info", "servers", "tags", "paths", "components"},
		slices.Sorted(maps.Keys(document))...)
	var output []byte
	for _, section := range sections {
		value, exists := document[section]
		if !exists {
			continue
		}
		data, err := yaml.Marshal(map[string]any{section: value})
		if err != nil {
			return nil, err
		}
		output = append(output, data...)
		delete(document, section)
	}
	return output, nil
}
