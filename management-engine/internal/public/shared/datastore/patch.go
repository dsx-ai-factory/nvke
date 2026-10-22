// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package datastore provides guarded spec patches and safe diagnostics for Kubernetes-backed APIs.
package datastore

import (
	"encoding/json"

	jsonpatch "github.com/evanphx/json-patch/v5"
)

// SpecMergePatch preserves unrelated fields and rejects writes after any intervening resource change.
// Callers must supply a validated, nonempty resource version from the stored object.
func SpecMergePatch(version string, before, after map[string]any) ([]byte, error) {
	previous, err := json.Marshal(map[string]any{"spec": before})
	if err != nil {
		return nil, err
	}
	// Include the read version even when the writable spec has not changed.
	next, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": version}, "spec": after,
	})
	if err != nil {
		return nil, err
	}
	return jsonpatch.CreateMergePatch(previous, next)
}
