// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package utils

import (
	"strings"
)

// Indent adds the specified number of spaces to the beginning of each non-empty line.
// Empty lines remain empty (no spaces added) to avoid trailing whitespace.
// Useful for indenting multi-line content in Go templates, particularly for YAML literal blocks.
//
// Example Go Template Usage:
//
//	template.New("...").Funcs(template.FuncMap{
//	    "indent": utils.Indent,
//	}).Parse(string(userDataTemplateBytes))
//
// Example Go Template File Usage:
//
//	{{ indent 10 .TrustBundle }}
func Indent(spaces int, value string) string {
	if value == "" {
		return ""
	}
	padding := strings.Repeat(" ", spaces)

	lines := strings.Split(value, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = padding + line
		}
	}

	return strings.Join(lines, "\n")
}

// TrimPrefix returns s with the leading prefix removed.
// Useful in templates when a value may have an optional prefix (e.g., "v" in "v1.33.6").
//
// Example: {{ trimPrefix .KubernetesVersion "v" }} -> "1.33.6" when input is "v1.33.6"
func TrimPrefix(s, prefix string) string {
	return strings.TrimPrefix(s, prefix)
}
