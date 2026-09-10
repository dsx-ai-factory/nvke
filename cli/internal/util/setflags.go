// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ApplySetFlags merges dot-notation "key=value" --set entries onto dst, an
// existing pointer to a JSON-tagged struct. Each key is dot notation
// matching dst's json field names (e.g. "controlPlaneConfig.nico.subnetIDs").
// Values are parsed as YAML scalars, then merged into dst by JSON-decoding
// with unknown fields disallowed, so dst's own json tags decide which keys
// are valid and what type each value must be.
//
// Fields no entry mentions keep their prior value on dst; later entries for
// the same key override earlier ones.
//
// arrayKeys lists the dot-paths whose value is always a JSON array, even a
// single value with no comma.
func ApplySetFlags[T any](dst *T, entries []string, arrayKeys map[string]bool) error {
	if len(entries) == 0 {
		return nil
	}

	patch := map[string]interface{}{}
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			return fmt.Errorf("--set %q: expected key=value", entry)
		}
		parsed, err := parseSetValue(key, value, arrayKeys)
		if err != nil {
			return fmt.Errorf("--set %q: %w", entry, err)
		}
		if err := setPath(patch, strings.Split(key, "."), parsed); err != nil {
			return fmt.Errorf("--set %q: %w", entry, err)
		}
	}

	data, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("encoding --set values: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("applying --set: %w", err)
	}
	return nil
}

// setPath writes value into the nested map at the dot-separated path,
// creating intermediate maps as needed.
func setPath(m map[string]interface{}, path []string, value interface{}) error {
	if path[0] == "" {
		return fmt.Errorf("empty key")
	}
	if len(path) == 1 {
		m[path[0]] = value
		return nil
	}
	next, ok := m[path[0]].(map[string]interface{})
	if !ok {
		next = map[string]interface{}{}
		m[path[0]] = next
	}
	return setPath(next, path[1:], value)
}

// parseSetValue coerces a --set value's raw text into the JSON type it most
// likely represents. Keys in arrayKeys always become an array, splitting on
// comma (even a single value with no comma becomes a one-element array).
// Otherwise the whole value is parsed as one YAML scalar, so "true"/"false"
// become booleans, bare integers become numbers, and everything else stays
// a string.
func parseSetValue(key, raw string, arrayKeys map[string]bool) (interface{}, error) {
	if arrayKeys[key] {
		parts := strings.Split(raw, ",")
		out := make([]interface{}, len(parts))
		for i, p := range parts {
			v, err := parseScalar(p)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	}
	return parseScalar(raw)
}

// parseScalar parses one YAML scalar, e.g. "true", "3", "16Gi", or a UUID
// string, into the Go value it represents (bool, int, float64, or string).
func parseScalar(raw string) (interface{}, error) {
	var v interface{}
	if err := yaml.Unmarshal([]byte(strings.TrimSpace(raw)), &v); err != nil {
		return nil, fmt.Errorf("parsing %q: %w", raw, err)
	}
	return v, nil
}
