// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// NodePoolReleaseSpec is the published content of a worker release: the machine
// image its nodes boot and the initialization applied to them.
// +kubebuilder:validation:XValidation:rule="has(self.profile) == has(self.tag)",message="profile and tag must be set together"
// +kubebuilder:validation:XValidation:rule="has(self.profile) == has(oldSelf.profile)",message="profile is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.tag) == has(oldSelf.tag)",message="tag is immutable"
type NodePoolReleaseSpec struct {
	// Profile is the name half of the <profile>:<tag> node profile a pool selects
	// this release by. It is set together with tag and cannot change.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="profile is immutable"
	Profile string `json:"profile,omitempty"`
	// Tag is the version half of the <profile>:<tag> node profile a pool selects
	// this release by. It is set together with profile and cannot change.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="tag is immutable"
	Tag string `json:"tag,omitempty"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="machineImageRef is immutable"
	MachineImageRef MachineImageReference `json:"machineImageRef"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="machineInit is immutable"
	MachineInit MachineInit `json:"machineInit"`
	// +optional
	// +kubebuilder:default={}
	Lifecycle ReleaseLifecycle `json:"lifecycle"`
}

// NodePoolRelease is what an operator publishes to make a worker release
// available to the engine, and how that release is managed once published.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,path=nodepoolreleases,singular=nodepoolrelease
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=".spec.machineImageRef.name"
// +kubebuilder:printcolumn:name="Deprecated",type=boolean,JSONPath=".spec.lifecycle.deprecated"
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
