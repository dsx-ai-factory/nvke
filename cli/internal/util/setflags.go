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
// map[string]interface{} document. Later entries for the same key override
// earlier ones. Combine the result with another such document via
// jsonpatch.MergeMergePatches (RFC 7386, gopkg.in/evanphx/json-patch.v4), or
// marshal it directly to JSON.
func BuildPatch[T any](entries []string) (map[string]interface{}, error) {
	return buildPatch[T](entries, false)
}

// BuildPatchStrings is BuildPatch for --set-string: every value is taken as
// a literal string, bypassing the YAML scalar coercion BuildPatch applies
// (so "3", "true", and "1.31" all stay strings). Comma-separated list
// elements are also left as strings.
func BuildPatchStrings[T any](entries []string) (map[string]interface{}, error) {
	return buildPatch[T](entries, true)
}

func buildPatch[T any](entries []string, forceString bool) (map[string]interface{}, error) {
	patch := map[string]interface{}{}
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("--set %q: expected key=value", entry)
		}
		path := canonicalPath[T](strings.Split(key, "."))
		var parsed interface{}
		var err error
		switch {
		case strings.TrimSpace(value) == "":
			parsed, err = emptyValueFor[T](path)
		case forceString:
			parsed, err = parseSetValueString(value, isArrayField[T](path))
		default:
			parsed, err = parseSetValue(value, isArrayField[T](path))
			if err == nil && parsed == nil {
				err = fmt.Errorf("YAML null is not a valid value; use an empty value to clear the field")
			}
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

// resolvedFieldKind walks T's struct tags along path's segments (descending
// through structs and pointers, unwrapping the final field's pointer too)
// and returns the Kind of the field found there. For example,
// "controlPlaneConfig.nico.subnetIDs" resolves to reflect.Slice, because
// NicoControlPlaneProviderUpdate.SubnetIDs is []uuid.UUID; so does
// "controlPlaneConfig.nodeLabels", despite being *[]string. An unresolvable
// path (unknown key, or a segment that isn't a struct) returns
// reflect.Invalid.
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
// field, an empty string for a string field or an unresolvable path. This is
// what --set key= (nothing after =) becomes. Returns an error for a field
// kind with no empty representation (bool, numeric, struct), since --set
// key= cannot clear those.
//
// Correct only when the caller marshals BuildPatch's result directly onto
// the wire. Decoding it onto a struct field whose type carries omitempty,
// or onto an already-populated map field, does not produce a clear.
func emptyValueFor[T any](path []string) (interface{}, error) {
	switch k := resolvedFieldKind[T](path); k {
	case reflect.Slice:
		return []interface{}{}, nil
	case reflect.Map:
		return map[string]interface{}{}, nil
	case reflect.String, reflect.Invalid:
		return "", nil
	default:
		return nil, fmt.Errorf("field cannot be cleared (%s has no empty value)", k)
	}
}

// canonicalPath rewrites path's segments to T's exact json tag casing,
// wherever the walk resolves a struct field — so a --set key typed in the
// wrong case still reaches the wire under the schema's real key. A segment
// past the schema-typed region (unresolvable, or under a free-form map
// field) is left as typed, matching resolvedFieldKind's behavior there.
func canonicalPath[T any](path []string) []string {
	canon := append([]string(nil), path...)
	t := reflect.TypeOf(*new(T))
	for i, seg := range path {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			break
		}
		field, ok := fieldByJSONTag(t, seg)
		if !ok {
			break
		}
		canon[i] = strings.Split(field.Tag.Get("json"), ",")[0]
		t = field.Type
	}
	return canon
}

// fieldByJSONTag finds the field of struct type t whose json tag name (the
// part before any comma) matches name case-insensitively.
func fieldByJSONTag(t reflect.Type, name string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		tag := strings.Split(field.Tag.Get("json"), ",")[0]
		if strings.EqualFold(tag, name) {
			return field, true
		}
	}
	return reflect.StructField{}, false
}

// setPath writes value into the nested map at the dot-separated path,
// creating intermediate maps as needed. Returns an error if an earlier
// entry conflicts with this one on the same path segment — either an
// earlier entry set a scalar where this one needs to descend into an
// object, or this one would overwrite nested values an earlier entry set.
func setPath(m map[string]interface{}, path []string, value interface{}) error {
	if path[0] == "" {
		return fmt.Errorf("empty key")
	}
	if len(path) == 1 {
		if existing, ok := m[path[0]].(map[string]interface{}); ok && len(existing) > 0 {
			return fmt.Errorf("%q already has nested values set by an earlier --set entry, cannot also set it directly", path[0])
		}
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
// type it most likely represents. When isArray is false, the value is
// parsed as one YAML scalar: "true"/"false" become booleans, bare integers
// become numbers, everything else stays a string.
//
// When isArray is true, the whole value is first tried as one YAML
// document, so sequence syntax like "[{path: /etc/foo, content: bar}]"
// parses directly into a list. Otherwise it falls back to a
// comma-separated list of scalars (e.g. "id1,id2"), with a single value
// becoming a one-element list; a literal comma inside one element (e.g. a
// shell command's flags) needs \, (see splitUnescapedCommas) or the YAML
// sequence form instead, since a bare comma always splits. An empty element
// in the comma-separated form (a leading, trailing, or doubled comma) is
// rejected rather than silently becoming a null list entry. An empty raw
// never reaches this function — see emptyValueFor.
func parseSetValue(raw string, isArray bool) (interface{}, error) {
	if !isArray {
		return parseScalar(raw)
	}
	if v, err := parseScalar(raw); err == nil {
		if list, ok := v.([]interface{}); ok {
			return list, nil
		}
	}
	parts := splitUnescapedCommas(raw)
	out := make([]interface{}, len(parts))
	for i, p := range parts {
		v, err := parseScalar(p)
		if err != nil {
			return nil, err
		}
		if v == nil {
			return nil, fmt.Errorf("empty element at position %d in %q; wrap the value in YAML sequence syntax [a,b] if an element contains a comma, or escape it as \\,", i, raw)
		}
		out[i] = v
	}
	return out, nil
}

// parseSetValueString is parseSetValue for --set-string: no YAML scalar
// coercion at all, so every element (or the whole value, for a non-array
// field) stays exactly the string the caller typed, aside from unescaping
// \, in a comma-separated list. An empty element is still rejected — see
// parseSetValue.
func parseSetValueString(raw string, isArray bool) (interface{}, error) {
	if !isArray {
		return raw, nil
	}
	parts := splitUnescapedCommas(raw)
	out := make([]interface{}, len(parts))
	for i, p := range parts {
		if p == "" {
			return nil, fmt.Errorf("empty element at position %d in %q", i, raw)
		}
		out[i] = p
	}
	return out, nil
}

// splitUnescapedCommas splits raw on commas, treating \, as a literal comma
// within one element rather than a separator (and dropping the backslash).
// Any other backslash is left as-is.
func splitUnescapedCommas(raw string) []string {
	var parts []string
	var cur strings.Builder
	runes := []rune(raw)
	for i := 0; i < len(runes); i++ {
		if runes[i] == '\\' && i+1 < len(runes) && runes[i+1] == ',' {
			cur.WriteRune(',')
			i++
			continue
		}
		if runes[i] == ',' {
			parts = append(parts, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(runes[i])
	}
	parts = append(parts, cur.String())
	return parts
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
