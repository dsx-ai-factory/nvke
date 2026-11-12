// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// NodePoolReleaseSpec is the published content of a worker release: the machine
// image its nodes boot and the initialization applied to them.
// +kubebuilder:validation:XValidation:rule="has(self.profile) == has(oldSelf.profile)",message="profile presence is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.tag) == has(oldSelf.tag)",message="tag presence is immutable"
type NodePoolReleaseSpec struct {
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="machineImageRef is immutable"
	MachineImageRef MachineImageReference `json:"machineImageRef"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="machineInit is immutable"
	MachineInit MachineInit `json:"machineInit"`
	// +optional
	// +kubebuilder:default={}
	Lifecycle ReleaseLifecycle `json:"lifecycle"`
	// Profile identifies the tenant-facing profile family.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="profile is immutable"
	// +optional
	Profile ResourceName `json:"profile,omitempty"`
	// Tag identifies a revision within Profile.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="tag is immutable"
	// +optional
	Tag string `json:"tag,omitempty"`
}

// NodePoolRelease is what an operator publishes to make a worker release
// available to the engine, and how that release is managed once published.
// Its internal name will become NodeProfile once the NVKE controller REST API
// no longer depends on it. The public API already uses NodeProfile.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,path=nodepoolreleases,singular=nodepoolrelease
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=".spec.machineImageRef.name"
// +kubebuilder:printcolumn:name="Deprecated",type=boolean,JSONPath=".spec.lifecycle.deprecated"
// +nvke:public:name=PublicNodeProfile
// +nvke:public:path=/v1/node-profiles
// +nvke:public:metadata=name,creationTimestamp,deletionTimestamp,uid
type NodePoolRelease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NodePoolReleaseSpec `json:"spec"`
}

// NodePoolReleaseList is a list of published worker releases.
// +kubebuilder:object:root=true
type NodePoolReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodePoolRelease `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &NodePoolRelease{}, &NodePoolReleaseList{})
		return nil
	})
}
