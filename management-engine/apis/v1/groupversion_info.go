// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package v1 defines the persisted internal management-engine API.
// Public tenant and operator contracts are separately versioned projections.
// +kubebuilder:object:generate=true
// +groupName=internal.nke.nvidia.com
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupName separates internal storage from the public API groups.
const GroupName = "internal.nke.nvidia.com"

var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1"}

// GroupVersion is the initial served and storage version of the internal API.
var GroupVersion = SchemeGroupVersion

var SchemeBuilder = runtime.NewSchemeBuilder(func(scheme *runtime.Scheme) error {
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
})

// AddToScheme registers every resource kind and its list type.
var AddToScheme = SchemeBuilder.AddToScheme
