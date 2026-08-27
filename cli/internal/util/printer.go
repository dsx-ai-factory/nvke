// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package util provides shared utility functions for nkectl commands:
// output formatting, age display, and other common CLI helpers.
package util

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"time"

	"gopkg.in/yaml.v3"
)

// Format is the output format requested by the -o flag.
type Format string

const (
	FormatTable Format = "" // default: human-readable table or key-value
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
)

// ParseFormat validates the value of the -o flag.
func ParseFormat(s string) (Format, error) {
	switch Format(s) {
	case FormatTable, FormatJSON, FormatYAML:
		return Format(s), nil
	default:
		return FormatTable, fmt.Errorf("unknown output format %q: must be json or yaml", s)
	}
}

// Print marshals v as JSON or YAML to w, depending on f.
// When f is FormatTable it returns an error — callers should handle the
// table case themselves and only call Print for structured formats.
func Print(w io.Writer, f Format, v any) error {
	switch f {
	case FormatJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	case FormatYAML:
		// Marshal through JSON first so that json: struct tags are used for
		// field names. yaml.v3 falls back to lowercased Go field names when
		// yaml: tags are absent, which is the case for all generated API types.
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("marshalling to json: %w", err)
		}
		var m any
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("unmarshalling json: %w", err)
		}
		return yaml.NewEncoder(w).Encode(m)
	default:
		return fmt.Errorf("cliutil.Print called with table format: use the table printer instead")
	}
}

// PrintFields marshals v to a map via JSON and recursively prints every
// field as key: value lines, indented for nested objects. This lets
// describe commands show all API fields without hardcoding them — new
// fields added to the API appear automatically.
func PrintFields(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshalling response: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("unmarshalling to map: %w", err)
	}
	printMap(w, m, 0)
	return nil
}

func printMap(w io.Writer, m map[string]any, indent int) {
	prefix := ""
	for range indent {
		prefix += "  "
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v := m[k]
		switch val := v.(type) {
		case map[string]any:
			fmt.Fprintf(w, "%s%s:\n", prefix, k)
			printMap(w, val, indent+1)
		case []any:
			fmt.Fprintf(w, "%s%s:\n", prefix, k)
			for i, item := range val {
				if sub, ok := item.(map[string]any); ok {
					fmt.Fprintf(w, "%s  [%d]:\n", prefix, i)
					printMap(w, sub, indent+2)
				} else {
					fmt.Fprintf(w, "%s  - %v\n", prefix, item)
				}
			}
		case nil:
			fmt.Fprintf(w, "%s%s: null\n", prefix, k)
		default:
			fmt.Fprintf(w, "%s%s: %v\n", prefix, k, val)
		}
	}
}

// FormatAge returns a short human-readable duration since t, matching the
// bucketed format kubectl uses: seconds, minutes, hours, or days.
func FormatAge(t time.Time) string {
	d := max(time.Since(t).Round(time.Second), 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
