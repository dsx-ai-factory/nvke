// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// publicOpenAPI returns the OpenAPI document Swagger UI loads. Every mapping
// and sequence item marked x-fern-ignore is removed, which is the same surface
// Fern and Redocly omit.
func publicOpenAPI(src []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("parsing OpenAPI document: %w", err)
	}
	// The relative /api server is marked x-fern-ignore so the Fern reference
	// uses the absolute example URL. Swagger UI needs that server so Try it out
	// calls this process, so the servers list is left as written.
	stripFernIgnored(&doc, map[string]struct{}{"servers": {}})

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("encoding OpenAPI document: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encoding OpenAPI document: %w", err)
	}
	return buf.Bytes(), nil
}

// stripFernIgnored removes mappings and sequence items that carry
// x-fern-ignore: true. preserveKeys names mapping keys whose values are copied
// through unchanged; it applies only to this mapping, not to children.
// It reports whether anything was removed.
func stripFernIgnored(node *yaml.Node, preserveKeys map[string]struct{}) bool {
	switch node.Kind {
	case yaml.DocumentNode:
		removed := false
		for _, child := range node.Content {
			if stripFernIgnored(child, preserveKeys) {
				removed = true
			}
		}
		return removed
	case yaml.MappingNode:
		return stripFernIgnoredMapping(node, preserveKeys)
	case yaml.SequenceNode:
		return stripFernIgnoredSequence(node)
	case yaml.ScalarNode, yaml.AliasNode:
		return false
	default:
		return false
	}
}

func stripFernIgnoredMapping(node *yaml.Node, preserveKeys map[string]struct{}) bool {
	before := propertyNames(node)

	kept := make([]*yaml.Node, 0, len(node.Content))
	removed := false
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		if _, ok := preserveKeys[key.Value]; ok {
			kept = append(kept, key, val)
			continue
		}
		if fernIgnored(val) || (stripFernIgnored(val, nil) && emptyCollection(val)) {
			removed = true
			continue
		}
		kept = append(kept, key, val)
	}
	node.Content = kept

	// A schema that drops a property must drop it from `required` too, or the
	// document demands a property it no longer defines. Under
	// additionalProperties: false no payload can then satisfy the schema.
	if dropped := droppedNames(before, propertyNames(node)); len(dropped) > 0 && pruneRequired(node, dropped) {
		removed = true
	}
	return removed
}

// propertyNames returns the keys of node's `properties` mapping, or nil when
// node is not a schema with properties.
func propertyNames(node *yaml.Node) map[string]struct{} {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != "properties" || node.Content[i+1].Kind != yaml.MappingNode {
			continue
		}
		props := node.Content[i+1].Content
		names := make(map[string]struct{}, len(props)/2)
		for j := 0; j+1 < len(props); j += 2 {
			names[props[j].Value] = struct{}{}
		}
		return names
	}
	return nil
}

// droppedNames returns the names in before that are no longer in after.
func droppedNames(before, after map[string]struct{}) map[string]struct{} {
	var dropped map[string]struct{}
	for name := range before {
		if _, ok := after[name]; ok {
			continue
		}
		if dropped == nil {
			dropped = make(map[string]struct{}, len(before))
		}
		dropped[name] = struct{}{}
	}
	return dropped
}

// pruneRequired removes dropped names from node's `required` sequence, and the
// key itself when nothing is left, since `required: []` is not valid OpenAPI
// 3.0. It reports whether anything was removed.
func pruneRequired(node *yaml.Node, dropped map[string]struct{}) bool {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != "required" || node.Content[i+1].Kind != yaml.SequenceNode {
			continue
		}
		req := node.Content[i+1]
		kept := make([]*yaml.Node, 0, len(req.Content))
		removed := false
		for _, item := range req.Content {
			if _, ok := dropped[item.Value]; ok && item.Kind == yaml.ScalarNode {
				removed = true
				continue
			}
			kept = append(kept, item)
		}
		req.Content = kept
		if removed && len(kept) == 0 {
			node.Content = append(node.Content[:i], node.Content[i+2:]...)
		}
		return removed
	}
	return false
}

func stripFernIgnoredSequence(node *yaml.Node) bool {
	kept := make([]*yaml.Node, 0, len(node.Content))
	removed := false
	for _, item := range node.Content {
		if fernIgnored(item) || (stripFernIgnored(item, nil) && emptyCollection(item)) {
			removed = true
			continue
		}
		kept = append(kept, item)
	}
	node.Content = kept
	return removed
}

// fernIgnored reports whether node is a mapping with x-fern-ignore set to
// boolean true. YAML accepts true, True, and TRUE. A quoted string does not
// count.
func fernIgnored(node *yaml.Node) bool {
	if node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		if key.Value != "x-fern-ignore" || val.Kind != yaml.ScalarNode || val.Tag != "!!bool" {
			continue
		}
		var ignored bool
		if err := val.Decode(&ignored); err != nil {
			return false
		}
		return ignored
	}
	return false
}

func emptyCollection(node *yaml.Node) bool {
	switch node.Kind {
	case yaml.MappingNode, yaml.SequenceNode:
		return len(node.Content) == 0
	case yaml.DocumentNode, yaml.ScalarNode, yaml.AliasNode:
		return false
	default:
		return false
	}
}
