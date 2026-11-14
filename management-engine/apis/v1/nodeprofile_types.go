// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// NodeProfileSpec is the published content of a worker release: the machine
// image its nodes boot and the initialization applied to them.
// +nvke:catalog:view
type NodeProfileSpec struct {
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="machineImageRef is immutable"
	MachineImageRef MachineImageReference `json:"machineImageRef"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="machineInit is immutable"
	MachineInit MachineInit `json:"machineInit"`
	// +optional
	// +kubebuilder:default={}
	// +nvke:catalog:include
	Lifecycle ReleaseLifecycle `json:"lifecycle"`
	// Profile identifies the tenant-facing profile family.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="profile is immutable"
	// +nvke:catalog:include
	Profile ResourceName `json:"profile"`
	// Tag identifies a revision within Profile.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="tag is immutable"
	// +nvke:catalog:include
	Tag string `json:"tag"`
}

// NodeProfile is what an operator publishes to make a worker release
// available to the engine, and how that release is managed once published.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,path=nodeprofiles,singular=nodeprofile
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=".spec.machineImageRef.name"
// +kubebuilder:printcolumn:name="Deprecated",type=boolean,JSONPath=".spec.lifecycle.deprecated"
// +nvke:public:name=PublicNodeProfile
// +nvke:public:path=/v1/node-profiles
// +nvke:public:metadata=name,creationTimestamp,deletionTimestamp,uid
type NodeProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NodeProfileSpec `json:"spec"`
}

// NodeProfileList is a list of published worker releases.
// +kubebuilder:object:root=true
type NodeProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &NodeProfile{}, &NodeProfileList{})
		return nil
	})
}
