// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// BuildPatch parses dot-notation "key=value" --set entries (matching T's
// json field names, e.g. "controlPlaneConfig.nico.subnetIDs") into a nested
// map[string]interface{} document. Field types come from T's own json tags
// and field kinds (via resolvedFieldKind), not from any hand-maintained
// list of keys, so adding a field to T needs no change here.
//
// Later entries for the same key override earlier ones. The result is
// meant to be merged with another such document (see MergeMap) or
// marshaled straight to JSON — callers that need the request as a typed
// Go value can json.Marshal the result and json.Decode it onto T; going
// through the map first (rather than decoding each entry directly onto T)
// is what lets a caller send the merged document itself as the request
// body, preserving an explicit empty value instead of a struct re-marshal
// collapsing it to the same wire representation as an omitted field via
// omitempty.
func BuildPatch[T any](entries []string) (map[string]interface{}, error) {
	patch := map[string]interface{}{}
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("--set %q: expected key=value", entry)
		}
		path := strings.Split(key, ".")
		var parsed interface{}
		var err error
		if value == "" {
			parsed = emptyValueFor[T](path)
		} else {
			parsed, err = parseSetValue(value, isArrayField[T](path))
		}
		if err != nil {
			return nil, fmt.Errorf("--set %q: %w", entry, err)
		}
		if err := setPath(patch, path, parsed); err != nil {
			return nil, fmt.Errorf("--set %q: %w", entry, err)
		}
	}
	return patch, nil
}

// MergeMap deep-merges src onto dst in place: when both dst[key] and
// src[key] are nested objects, they merge recursively (so --set can touch
// one field under a path while a -f document's other fields under the same
// path survive); otherwise src[key] replaces dst[key] entirely, including
// when the value is an array — arrays are never merged element-wise, only
// ever replaced as a whole.
func MergeMap(dst, src map[string]interface{}) {
	for k, v := range src {
		if srcMap, ok := v.(map[string]interface{}); ok {
			if dstMap, ok := dst[k].(map[string]interface{}); ok {
				MergeMap(dstMap, srcMap)
				continue
			}
		}
		dst[k] = v
	}
}

// resolvedFieldKind walks T's struct tags along path's segments (descending
// through structs and pointers, unwrapping the final field's pointer too),
// and returns the Kind of the field found there — e.g.
// "controlPlaneConfig.nico.subnetIDs" resolves to reflect.Slice because
// NicoControlPlaneProviderUpdate.SubnetIDs is []uuid.UUID, and
// "controlPlaneConfig.nodeLabels" also resolves to reflect.Slice despite
// being *[]string. An unresolvable path (unknown key, or a segment that
// isn't a struct) returns reflect.Invalid — the caller's own decode of the
// final document is what actually rejects an unknown key.
func resolvedFieldKind[T any](path []string) reflect.Kind {
	t := reflect.TypeOf(*new(T))
	for _, seg := range path {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return reflect.Invalid
		}
		field, ok := fieldByJSONTag(t, seg)
		if !ok {
			return reflect.Invalid
		}
		t = field.Type
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind()
}

// isArrayField reports whether T's field at path is a slice (or a pointer
// to one). See resolvedFieldKind.
func isArrayField[T any](path []string) bool {
	return resolvedFieldKind[T](path) == reflect.Slice
}

// emptyValueFor returns the JSON-empty value matching the Kind of T's field
// at path: an empty array for a slice field, an empty object for a map
// field, an empty string otherwise. This is what --set key= (nothing after
// =) becomes, so a field whose API contract treats an empty value as
// "clear this field" — as opposed to omitting the key, which leaves it
// alone — can be reached through --set with the type its own field
// actually needs, not just a bare YAML null.
//
// This only produces the right wire behavior for a caller that marshals
// BuildPatch's result directly and sends those bytes as the request body.
// A caller that instead decodes to build a Go value first has two separate
// traps to avoid: decoding this empty value onto a *struct field whose type
// still has omitempty* re-collapses it to "omitted" on any later
// re-marshal of that struct; decoding it onto an *already-populated map
// field* is a no-op, because encoding/json merges a JSON object onto an
// existing map instead of replacing it.
func emptyValueFor[T any](path []string) interface{} {
	switch resolvedFieldKind[T](path) {
	case reflect.Slice:
		return []interface{}{}
	case reflect.Map:
		return map[string]interface{}{}
	default:
		return ""
	}
}

// fieldByJSONTag finds the field of struct type t whose json tag name (the
// part before any comma) matches name.
func fieldByJSONTag(t reflect.Type, name string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if strings.Split(field.Tag.Get("json"), ",")[0] == name {
			return field, true
		}
	}
	return reflect.StructField{}, false
}

// setPath writes value into the nested map at the dot-separated path,
// creating intermediate maps as needed. It errors rather than silently
// overwriting when an earlier entry already put a scalar at a path segment
// this one needs to descend through (e.g. --set controlPlaneConfig=x before
// --set controlPlaneConfig.nico.osProfile=y).
func setPath(m map[string]interface{}, path []string, value interface{}) error {
	if path[0] == "" {
		return fmt.Errorf("empty key")
	}
	if len(path) == 1 {
		m[path[0]] = value
		return nil
	}
	existing, exists := m[path[0]]
	if !exists {
		next := map[string]interface{}{}
		m[path[0]] = next
		return setPath(next, path[1:], value)
	}
	next, ok := existing.(map[string]interface{})
	if !ok {
		return fmt.Errorf("%q was already set to a scalar value by an earlier --set entry, cannot also set %q", path[0], strings.Join(path, "."))
	}
	return setPath(next, path[1:], value)
}

// parseSetValue coerces a non-empty --set value's raw text into the JSON
// type it most likely represents. When isArray is false the whole value is
// parsed as one YAML scalar, so "true"/"false" become booleans, bare
// integers become numbers, and everything else stays a string.
//
// When isArray is true, the whole value is first tried as one YAML
// document: real sequence syntax like "[{path: /etc/foo, content: bar}]"
// parses directly into a list, which is what an array of objects (e.g.
// bootstrapFiles) needs. If that doesn't parse, or parses to something
// other than a list, raw falls back to a comma-separated list of scalars
// (e.g. "id1,id2"), still becoming a one-element list for a single value
// with no comma. An empty raw never reaches this function — see
// emptyValueFor.
func parseSetValue(raw string, isArray bool) (interface{}, error) {
	if !isArray {
		return parseScalar(raw)
	}
	if v, err := parseScalar(raw); err == nil {
		if list, ok := v.([]interface{}); ok {
			return list, nil
		}
	}
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

// parseScalar parses one YAML scalar, e.g. "true", "3", "16Gi", or a UUID
// string, into the Go value it represents (bool, int, float64, or string).
func parseScalar(raw string) (interface{}, error) {
	var v interface{}
	if err := yaml.Unmarshal([]byte(strings.TrimSpace(raw)), &v); err != nil {
		return nil, fmt.Errorf("parsing %q: %w", raw, err)
	}
	return v, nil
}
