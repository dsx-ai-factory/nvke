// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cwe

import (
	"bytes"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// YAMLToRequest reads a YAML file and deserialises it into a CWE API request
// type T. Field names in the YAML must match the json tags of T exactly.
//
// The conversion goes YAML → interface{} → JSON → T so that the json tags on
// the generated API types are honoured. Direct yaml.Unmarshal into generated
// structs is avoided because those structs only carry json tags, not yaml tags.
func YAMLToRequest[T any](data []byte) (T, error) {
	var raw interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return *new(T), fmt.Errorf("unmarshal yaml: %w", err)
	}
	j, err := json.Marshal(raw)
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
