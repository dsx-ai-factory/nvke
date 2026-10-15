// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// OpenAPI annotations project approved constraints so client tooling can discover public input requirements.

package main

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

const openAPIOptions = "grpc.gateway.protoc_gen_openapiv2.options."

// renderRequiredFields emits required names visible to one operation so private requirements stay unpublished.
func renderRequiredFields(out *bytes.Buffer, entry *node, mode string) {
	var required []string
	for _, child := range entry.children {
		if child.Required && slices.Contains(child.Access, mode) {
			required = append(required, strconv.Quote(child.Name))
		}
	}
	if len(required) != 0 {
		list := "[" + required[0] + "]"
		if len(required) > 1 {
			list = "[\n        " + strings.Join(required, ",\n        ") + "\n      ]"
		}
		fmt.Fprintf(out, "  option (%sopenapiv2_schema) = {\n    json_schema: {\n      required: %s\n    }\n  };\n\n",
			openAPIOptions, list)
	}
}

// fieldConstraints emits supported scalar and list annotations so public metadata excludes private schema rules.
func fieldConstraints(schema apiextensionsv1.JSONSchemaProps) []string {
	var options []string
	if schema.Type == schemaArray {
		options = appendBounds(options, "min_items", schema.MinItems, "max_items", schema.MaxItems)
		// grpc-gateway places string constraints on a repeated scalar's items schema.
		schema = *schema.Items.Schema
	}
	if schema.Type == schemaString {
		options = appendBounds(options, "min_length", schema.MinLength, "max_length", schema.MaxLength)
		if schema.Pattern != "" {
			options = append(options, "pattern: "+strconv.Quote(schema.Pattern))
		}
	}
	return options
}

// appendBounds emits only present limits so absent CRD bounds do not become invented public constraints.
func appendBounds(options []string, minName string, minimum *int64, maxName string, maximum *int64) []string {
	if minimum != nil {
		options = append(options, fmt.Sprintf("%s: %d", minName, *minimum))
	}
	if maximum != nil {
		options = append(options, fmt.Sprintf("%s: %d", maxName, *maximum))
	}
	return options
}
