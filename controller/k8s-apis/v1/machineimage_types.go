// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// MachineImageSpec is the immutable identity of a bootable machine image and
// the boot contract a provider needs to launch it. The whole spec is immutable.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="machine image spec is immutable; publish a new name instead"
type MachineImageSpec struct {
	OperatingSystem OperatingSystem `json:"operatingSystem"`
	// Architecture is a Go/OCI architecture name such as "amd64" or "arm64".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Architecture string `json:"architecture"`
	// Variant distinguishes images built from the same operating system for a
	// different purpose, such as "fde" for full disk encryption.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Variant string `json:"variant,omitempty"`
	// BuildDate is the date the image was built, as YYYY-MM-DD.
	// +kubebuilder:validation:Format=date
	BuildDate string `json:"buildDate"`
	// NICo holds the boot contract of the NICo infrastructure provider.
	NICo NICoBoot `json:"nico"`
}

// OperatingSystem identifies the distribution an image was built from.
type OperatingSystem struct {
	// Family is the distribution name, such as "ubuntu".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Family string `json:"family"`
	// Version is the distribution version, such as "24.04".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)*$`
	Version string `json:"version"`
}

// NICoBoot is how NICo boots one particular image.
type NICoBoot struct {
	// IPXEScript is a Go template rendered with machine-specific values, such as
	// the disk selection strategy, and the result becomes the iPXE script of the
	// NICo machine resource.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=16384
	// +kubebuilder:validation:XValidation:rule="self.startsWith('#!ipxe\\n') || self.startsWith('#!ipxe\\r\\n')",message="ipxeScript must begin with a #!ipxe line of its own"
	// +kubebuilder:validation:XValidation:rule="self.replace('#!ipxe', '').trim() != ''",message="ipxeScript must contain a command after the #!ipxe line"
	// +kubebuilder:validation:XValidation:rule="self.split('{{').size() == self.split('}}').size()",message="ipxeScript has unbalanced Go template delimiters"
	IPXEScript string `json:"ipxeScript"`
}

// MachineImage is an immutable node image published as engine configuration.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,path=machineimages,singular=machineimage
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="OS",type=string,JSONPath=".spec.operatingSystem.family"
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=".spec.operatingSystem.version"
// +kubebuilder:printcolumn:name="Arch",type=string,JSONPath=".spec.architecture"
// +kubebuilder:printcolumn:name="Variant",type=string,JSONPath=".spec.variant"
// +kubebuilder:printcolumn:name="Built",type=string,JSONPath=".spec.buildDate"
type MachineImage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MachineImageSpec `json:"spec"`
}

// MachineImageList is a list of published machine images.
// +kubebuilder:object:root=true
type MachineImageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MachineImage `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &MachineImage{}, &MachineImageList{})
		return nil
	})
}
