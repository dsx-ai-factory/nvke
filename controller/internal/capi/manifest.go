// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

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
