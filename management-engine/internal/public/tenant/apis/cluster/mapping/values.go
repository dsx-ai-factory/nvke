// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mapping validates public values and projects reviewed storage fields
// without exposing private fields or retaining caller-owned mutable values.
package mapping

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// FieldError reports a public field and a safe reason without echoing input values.
type FieldError struct {
	// Field identifies the public input to correct without exposing its value.
	Field string
	// Description gives a fixed validation reason suitable for a public error response.
	Description string
}

// Error formats the public field and fixed reason so diagnostics omit input values.
func (e *FieldError) Error() string { return e.Field + ": " + e.Description }

// required identifies an absent public field without including its surrounding input.
func required(path string) error {
	return &FieldError{Field: path, Description: "is required"}
}

// validateString checks character bounds before the compiled pattern to match CRD
// string semantics while returning only a public path and fixed failure reason.
func validateString(value, path string, minLength, maxLength int, pattern *regexp.Regexp) error {
	length := utf8.RuneCountInString(value)
	if length < minLength || (maxLength >= 0 && length > maxLength) {
		return &FieldError{Field: path, Description: "has invalid length"}
	}
	if pattern != nil && !pattern.MatchString(value) {
		return &FieldError{Field: path, Description: "has invalid format"}
	}
	return nil
}

// validateListSize enforces reviewed collection bounds before callers allocate or copy items.
func validateListSize(size int, path string, minItems, maxItems int) error {
	if size < minItems || (maxItems >= 0 && size > maxItems) {
		return &FieldError{Field: path, Description: "has invalid item count"}
	}
	return nil
}

// readString preserves scalar absence and hides accessor errors that may contain stored values.
func readString(src map[string]any, path ...string) (*string, error) {
	value, found, err := unstructured.NestedString(src, path...)
	if err != nil {
		return nil, fmt.Errorf("invalid stored field %s", strings.Join(path, "."))
	}
	if !found {
		return nil, nil
	}
	return &value, nil
}

// readStrings copies a stored string list to prevent projection mutations from changing storage.
// Malformed values produce only their reviewed field path in the error.
func readStrings(src map[string]any, path ...string) ([]string, error) {
	value, _, err := unstructured.NestedStringSlice(src, path...)
	if err != nil {
		return nil, fmt.Errorf("invalid stored field %s", strings.Join(path, "."))
	}
	return value, nil
}

// hasObject checks object presence without copying private siblings that projection will omit.
// Malformed values produce only their reviewed field path in the error.
func hasObject(src map[string]any, path ...string) (bool, error) {
	value, found, err := unstructured.NestedFieldNoCopy(src, path...)
	if err != nil {
		return false, fmt.Errorf("invalid stored field %s", strings.Join(path, "."))
	}
	if found {
		if _, ok := value.(map[string]any); !ok {
			return false, fmt.Errorf("invalid stored field %s", strings.Join(path, "."))
		}
	}
	return found, nil
}

// putValue copies generated JSON-compatible values into a caller-owned map so inputs cannot alias it.
// Invalid storage ancestry returns a field path without exposing stored values.
func putValue(dst map[string]any, value any, path ...string) error {
	if err := unstructured.SetNestedField(dst, value, path...); err != nil {
		return fmt.Errorf("invalid stored field %s", strings.Join(path, "."))
	}
	return nil
}

// replaceValue copies or clears one reviewed leaf from validated replacement values,
// preserving private siblings and create-only fields in the caller-owned spec copy.
func replaceValue(dst, values map[string]any, path ...string) error {
	value, found, err := unstructured.NestedFieldNoCopy(values, path...)
	if err != nil {
		return fmt.Errorf("read replacement field %s: %w", strings.Join(path, "."), err)
	}
	if !found {
		unstructured.RemoveNestedField(dst, path...)
		return nil
	}
	return putValue(dst, value, path...)
}
