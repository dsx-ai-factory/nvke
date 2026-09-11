// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"encoding/json"
	"fmt"

	k8syaml "sigs.k8s.io/yaml"
)

// YAMLToRequest reads a YAML document and deserialises it into a CWE API
// request type T, rejecting unknown fields. Field names in the YAML must
// match the json tags of T.
func YAMLToRequest[T any](data []byte) (T, error) {
	// Guard the document's shape first: k8syaml.UnmarshalStrict silently
	// decodes an empty document as a zero-value T instead of erroring.
	if _, err := YAMLToGenericMap(data); err != nil {
		return *new(T), err
	}
	var req T
	if err := k8syaml.UnmarshalStrict(data, &req); err != nil {
		return *new(T), fmt.Errorf("unmarshal request: %w", err)
	}
	return req, nil
}

// YAMLToGenericMap parses a YAML document into a map[string]interface{},
// keyed the same way json.Marshal would read it back out. Combine the
// result with another such map via jsonpatch.MergeMergePatches (RFC 7386,
// gopkg.in/evanphx/json-patch.v4), or marshal it directly to JSON.
func YAMLToGenericMap(data []byte) (map[string]interface{}, error) {
	j, err := k8syaml.YAMLToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("unmarshal yaml: %w", err)
	}
	var raw interface{}
	if err := json.Unmarshal(j, &raw); err != nil {
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
