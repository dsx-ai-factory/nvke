// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package v1 defines the NVKE controller's own Kubernetes API: the MachineImage,
// ClusterRelease and NodePoolRelease resources it reads to resolve releases.
//
// The NVKE controller owns these types.
// +kubebuilder:object:generate=true
// +groupName=internal.nvke.nvidia.com
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// GroupName identifies the persisted internal API.
	GroupName = "internal.nvke.nvidia.com"
)

var (
	// SchemeGroupVersion is the group and version of every kind in this package.
	SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1"} //nolint:gochecknoglobals // Kubernetes API registration requires package variables.

	// GroupVersion is the initial served and storage version of the internal API.
	GroupVersion = SchemeGroupVersion //nolint:gochecknoglobals // Kubernetes API registration requires package variables.

	// SchemeBuilder collects the registration functions of the kinds in this package.
	SchemeBuilder = runtime.NewSchemeBuilder(func(scheme *runtime.Scheme) error { //nolint:gochecknoglobals // Kubernetes API registration requires package variables.
		metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
		return nil
	})

	// AddToScheme registers every resource kind and its list type.
	AddToScheme = SchemeBuilder.AddToScheme //nolint:gochecknoglobals // Kubernetes API registration requires package variables.
)
