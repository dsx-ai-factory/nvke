// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 holds the API types of the provisioned Cluster API
// infrastructure provider.
//
// The provider creates no infrastructure. It binds each Cluster API machine to
// one host from an inventory that the operator supplies on the
// ProvisionedCluster, and it reports the address that the network already
// assigned to that host.
//
// This package must not import anything from the cloud-workflow-engine, so the
// provider can move to its own module or repository without a code change.
// +kubebuilder:object:generate=true
// +groupName=infrastructure.cluster.x-k8s.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupName is the API group. Cluster API requires every infrastructure
// provider to use this group, and the controller rejects a manifest that uses any other
// group.
const (
	GroupName = "infrastructure.cluster.x-k8s.io"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"} //nolint:gochecknoglobals // Kubernetes API registration requires package variables.

	// schemeBuilder is used to add go types to the GroupVersionKind scheme.
	schemeBuilder = runtime.NewSchemeBuilder(addKnownTypes) //nolint:gochecknoglobals // Kubernetes API registration requires package variables.

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = schemeBuilder.AddToScheme //nolint:gochecknoglobals // Kubernetes API registration requires package variables.

	objectTypes = []runtime.Object{} //nolint:gochecknoglobals // Kubernetes API registration requires package variables.
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion, objectTypes...)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
