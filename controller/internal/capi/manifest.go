// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"bytes"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	dockerMachineTemplateKind = "DockerMachineTemplate"
	infrastructureGroup       = "infrastructure.cluster.x-k8s.io"
)

func IsEmptyManifest(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("{}")) || bytes.Equal(trimmed, []byte("null"))
}

func IsDockerMachineTemplate(raw []byte) (bool, error) {
	template, err := ParseManifest(raw)
	if err != nil {
		return false, err
	}

	groupVersion, err := schema.ParseGroupVersion(template.GetAPIVersion())
	if err != nil {
		return false, fmt.Errorf("failed to parse apiVersion %q: %w", template.GetAPIVersion(), err)
	}

	return groupVersion.Group == infrastructureGroup && template.GetKind() == dockerMachineTemplateKind, nil
}
