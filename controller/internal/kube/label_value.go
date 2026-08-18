// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"strings"
)

// NormalizeLabelValue maps an arbitrary Forge name to a valid Kubernetes label
// value matching `[a-z0-9A-Z]([a-z0-9A-Z._-]*[a-z0-9A-Z])?` (<=63 chars). The
// transform is deterministic so the same input always yields the same label,
// keeping selectors byte-stable across re-runs.
func NormalizeLabelValue(s string) string {
	var ascii strings.Builder
	for _, r := range s {
		if r < 128 { // drop multi-byte/non-ASCII runes
			ascii.WriteRune(r)
		}
	}

	lowered := strings.ToLower(ascii.String())

	var replaced strings.Builder
	for _, r := range lowered {
		if isLabelValueByte(r) {
			replaced.WriteRune(r)
		} else {
			replaced.WriteByte('-') // spaces, slashes, etc.
		}
	}
	v := replaced.String()

	for strings.Contains(v, "--") {
		v = strings.ReplaceAll(v, "--", "-")
	}

	v = strings.TrimFunc(v, func(r rune) bool { return !isAlphaNum(r) })

	if len(v) > 63 {
		v = strings.TrimRightFunc(v[:63], func(r rune) bool { return !isAlphaNum(r) })
	}

	return v
}

func isLabelValueByte(r rune) bool {
	return isAlphaNum(r) || r == '.' || r == '_' || r == '-'
}

func isAlphaNum(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}
