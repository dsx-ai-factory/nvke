// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 defines the persisted internal management-engine API.
// Public tenant and operator contracts are separately versioned projections.
// +kubebuilder:object:generate=true
// +groupName=internal.nke.nvidia.com
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupName separates internal storage from the public API groups.
const GroupName = "internal.nke.nvidia.com"

// GroupVersion is the initial served and storage version of the internal API.
var GroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

// AddToScheme registers both resource kinds and their list types.
func AddToScheme(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&NKECluster{}, &NKEClusterList{},
		&NodePool{}, &NodePoolList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
