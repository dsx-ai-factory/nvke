// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// ErrNotMapping is returned when a YAML document is not a mapping at the top
// level, for example a list or a single value.
var ErrNotMapping = errors.New("yaml document is not a mapping")

// YAMLToRequest reads a YAML document and deserialises it into a CWE API
// request type T. Field names in the YAML must match the json tags of T.
func YAMLToRequest[T any](data []byte) (T, error) {
	m, err := YAMLToGenericMap(data)
	if err != nil {
		return *new(T), err
	}
	// Round-tripped through JSON rather than yaml.Unmarshal(data, &req)
	// directly: generated API types carry only json tags, not yaml tags.
	j, err := json.Marshal(m)
	if err != nil {
		return *new(T), fmt.Errorf("marshal json: %w", err)
	}
	var req T
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return *new(T), fmt.Errorf("unmarshal request: %w", err)
	}
	return req, nil
}

// keepTimestampText makes every scalar that YAML would read as a timestamp, such
// as the unquoted 2026-03-23, stay the text it was written as. Otherwise the
// decoder turns it into a time and JSON encoding renders it as
// "2026-03-23T00:00:00Z", which is not a date. JSON has no timestamps, so the
// text is what the document said.
func keepTimestampText(n *yaml.Node) {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!timestamp" {
		n.Tag = "!!str"
	}
	for _, c := range n.Content {
		keepTimestampText(c)
	}
}

// YAMLToGenericMap parses a single YAML document into a
// map[string]interface{}, keyed the same way json.Marshal would read it
// back out.
func YAMLToGenericMap(data []byte) (map[string]interface{}, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("empty document")
		}
		return nil, fmt.Errorf("unmarshal yaml: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("empty document")
	}

	// Try to decode a second document, to reject one if present.
	var extra interface{}
	switch err := dec.Decode(&extra); {
	case errors.Is(err, io.EOF):
		// Only one document — expected.
	case err == nil:
		return nil, fmt.Errorf("multiple YAML documents; only one is accepted")
	default:
		return nil, fmt.Errorf("unmarshal yaml: %w", err)
	}

	root := doc.Content[0]
	if root.Tag == "!!null" {
		return nil, fmt.Errorf("empty document")
	}
	if root.Kind != yaml.MappingNode {
		return nil, ErrNotMapping
	}
	keepTimestampText(root)
	if err := validateStringKeys(root); err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err := root.Decode(&result); err != nil {
		return nil, fmt.Errorf("unmarshal yaml: %w", err)
	}
	return result, nil
}

// validateStringKeys checks the YAML syntax tree before decoding it into Go
// values, avoiding recursive inspection and rewriting of decoded maps.
func validateStringKeys(node *yaml.Node) error {
	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.ShortTag() != "!!str" && key.ShortTag() != "!!merge" {
				return fmt.Errorf("yaml document keys must be strings, found %s (%s)", key.Value, key.ShortTag())
			}
		}
	}
	for _, child := range node.Content {
		if err := validateStringKeys(child); err != nil {
			return err
		}
	}
	return nil
}
