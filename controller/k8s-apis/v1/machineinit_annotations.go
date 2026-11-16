// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

const (
	// MachineInitKubernetesVersionAnnotation records the Kubernetes version a
	// fixed-version release boots. It is empty when
	// MachineInitKubernetesVersionInjectedAnnotation is "true".
	MachineInitKubernetesVersionAnnotation = GroupName + "/machine-init-kubernetes-version"

	// MachineInitKubernetesVersionInjectedAnnotation is "true" when the release
	// accepts a Kubernetes version at provisioning time.
	MachineInitKubernetesVersionInjectedAnnotation = GroupName + "/machine-init-kubernetes-version-injected"
)
