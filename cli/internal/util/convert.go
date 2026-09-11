// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"bytes"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// YAMLToRequest reads a YAML document and deserialises it into a CWE API
// request type T. Field names in the YAML must match the json tags of T.
//
// The conversion goes YAML → interface{} → JSON → T so that json tags on
// generated API types are honoured. Direct yaml.Unmarshal is avoided because
// generated structs only carry json tags, not yaml tags.
func YAMLToRequest[T any](data []byte) (T, error) {
	m, err := YAMLToGenericMap(data)
	if err != nil {
		return *new(T), err
	}
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

// YAMLToGenericMap parses a YAML document into a map[string]interface{},
// keyed the same way json.Marshal would read it back out.
func YAMLToGenericMap(data []byte) (map[string]interface{}, error) {
	var raw interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal yaml: %w", err)
	}
	if raw == nil {
		return nil, fmt.Errorf("empty document")
	}
	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("yaml document is not a mapping")
	}
	return m, nil
}
