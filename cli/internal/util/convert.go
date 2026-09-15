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

// YAMLToGenericMap parses a single YAML document into a
// map[string]interface{}, keyed the same way json.Marshal would read it
// back out. Rejects a second "---"-separated document in data, and a
// mapping with a non-string key at any depth.
func YAMLToGenericMap(data []byte) (map[string]interface{}, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var raw interface{}
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("empty document")
		}
		return nil, fmt.Errorf("unmarshal yaml: %w", err)
	}
	if raw == nil {
		return nil, fmt.Errorf("empty document")
	}

	var extra interface{}
	switch err := dec.Decode(&extra); {
	case errors.Is(err, io.EOF):
		// Only one document — expected.
	case err == nil:
		return nil, fmt.Errorf("multiple YAML documents; only one is accepted")
	default:
		return nil, fmt.Errorf("unmarshal yaml: %w", err)
	}

	normalized, err := stringKeyed(raw)
	if err != nil {
		return nil, err
	}
	m, ok := normalized.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("yaml document is not a mapping")
	}
	return m, nil
}

// stringKeyed recursively converts every map[interface{}]interface{} under
// v (gopkg.in/yaml.v3's representation of a mapping with any non-string
// key) to map[string]interface{}. Returns an error naming the first
// non-string key found, at any depth.
func stringKeyed(v interface{}) (interface{}, error) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			converted, err := stringKeyed(val)
			if err != nil {
				return nil, err
			}
			t[k] = converted
		}
		return t, nil
	case map[interface{}]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, val := range t {
			key, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("yaml document keys must be strings, found %v (%T)", k, k)
			}
			converted, err := stringKeyed(val)
			if err != nil {
				return nil, err
			}
			out[key] = converted
		}
		return out, nil
	case []interface{}:
		for i, val := range t {
			converted, err := stringKeyed(val)
			if err != nil {
				return nil, err
			}
			t[i] = converted
		}
		return t, nil
	default:
		return v, nil
	}
}
