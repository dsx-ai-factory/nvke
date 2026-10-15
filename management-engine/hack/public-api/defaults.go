// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Private create defaults fill required stored fields without adding them to the public input surface.
// Provisioning rationale is documented in public/tenant/README.md under "Private create values".

package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// compileDefaults checks explicit private create values so every required stored field has a known source.
func compileDefaults(
	model *model,
	defaults []defaultValue,
	spec *apiextensionsv1.JSONSchemaProps,
	indexed map[string]*node,
) error {
	model.createAdmission = map[string]record{}
	present := map[string]bool{"": true}
	for path, node := range indexed {
		if slices.Contains(node.Access, modeCreate) {
			present[path] = true
		}
	}
	slices.SortFunc(defaults, func(a, b defaultValue) int { return strings.Compare(a.Path, b.Path) })
	for _, value := range defaults {
		if !pathPattern.MatchString(value.Path) || present[value.Path] {
			return fmt.Errorf("invalid, duplicate, or create-writable default path %s", value.Path)
		}
		parts := strings.Split(value.Path, ".")
		schema, required, err := resolveSchema(spec, parts)
		if err != nil {
			return err
		}
		var literal any
		if err := json.Unmarshal(value.Value, &literal); err != nil {
			return fmt.Errorf("decode private default %s: %w", value.Path, err)
		}
		switch literal.(type) {
		case string:
			if schema.Type != schemaString {
				return fmt.Errorf("private default %s is not a stored string", value.Path)
			}
		case bool:
			if schema.Type != "boolean" {
				return fmt.Errorf("private default %s is not a stored boolean", value.Path)
			}
		default:
			return fmt.Errorf("private default %s must be an explicit string or boolean", value.Path)
		}
		value.Value, err = json.Marshal(literal)
		if err != nil {
			return fmt.Errorf("encode private default %s: %w", value.Path, err)
		}
		model.defaults = append(model.defaults, value)
		model.createAdmission["$default:"+value.Path] = record{
			Name: "private_create_default", Required: required, Schema: constraintsOnly(schema), Default: value.Value,
		}
		for i := 1; i <= len(parts); i++ {
			present[strings.Join(parts[:i], ".")] = true
		}
	}
	for path := range present {
		schema := spec
		if path != "" {
			var err error
			schema, _, err = resolveSchema(spec, strings.Split(path, "."))
			if err != nil {
				return err
			}
		}
		if schema.Type != schemaObject {
			continue
		}
		if _, exposed := indexed[path]; !exposed {
			model.createAdmission["$create:"+path] = record{Name: "private_create_admission", Schema: constraintsOnly(schema)}
		}
		for _, name := range schema.Required {
			child := name
			if path != "" {
				child = path + "." + name
			}
			if !present[child] {
				return fmt.Errorf("required stored field %s has no public create field or explicit default", child)
			}
		}
	}
	return nil
}
