// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"bytes"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	capiInfrastructureGroup = "infrastructure.cluster.x-k8s.io"
)

// isEmptyManifestBytes mirrors internal/capi.IsEmptyManifest. It's
// duplicated locally rather than imported because internal/capi already
// imports internal/clients (for its ClusterScaffold wrapper), and importing
// it back here would create a cycle.
func isEmptyManifestBytes(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("{}")) || bytes.Equal(trimmed, []byte("null"))
}

// ValidateInfrastructureManifestBytes validates a raw CAPI infra
// machine/cluster manifest: it must carry an apiVersion and kind, and its
// apiVersion group must be a CAPI infrastructure provider group. An empty
// manifest is valid — the caller decides whether a manifest is required at
// all. Exported because the cluster-level infra.cluster manifest has no
// db.NodePool/db.ControlPlaneConfigSpec equivalent to carry it through
// ValidateNodePoolPayload/ValidateControlPlaneConfigPayload, and no Nico
// counterpart exists to dispatch against — this check is CAPI-only.
func ValidateInfrastructureManifestBytes(raw json.RawMessage) error {
	if isEmptyManifestBytes(raw) {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return fmt.Errorf("failed to unmarshal manifest: %w", err)
	}
	return validateInfrastructureManifestObject(obj)
}

func validateInfrastructureManifestObject(obj map[string]any) error {
	if len(obj) == 0 {
		return nil
	}

	manifest := &unstructured.Unstructured{Object: obj}
	apiVersion := manifest.GetAPIVersion()
	if apiVersion == "" {
		return fmt.Errorf("apiVersion is required")
	}
	if manifest.GetKind() == "" {
		return fmt.Errorf("kind is required")
	}

	groupVersion, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return fmt.Errorf("apiVersion %q is invalid: %w", apiVersion, err)
	}
	if groupVersion.Group != capiInfrastructureGroup {
		return fmt.Errorf(
			"apiVersion group %q must be a CAPI infrastructure provider group such as %q",
			groupVersion.Group,
			capiInfrastructureGroup,
		)
	}

	return nil
}
