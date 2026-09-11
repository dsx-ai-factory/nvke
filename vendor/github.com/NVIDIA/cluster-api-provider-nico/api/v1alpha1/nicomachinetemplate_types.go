// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// NicoMachineTemplateSpec defines the desired state of NicoMachineTemplate.
type NicoMachineTemplateSpec struct {
	Template NicoMachineTemplateResource `json:"template"`
}

// NicoMachineTemplateResource describes the data needed to create a NicoMachine from a template.
type NicoMachineTemplateResource struct {
	// Standard object's metadata.
	// +optional
	ObjectMeta clusterv1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.template.spec is immutable"
	Spec NicoMachineSpec `json:"spec"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories=cluster-api,shortName=nicomt

// NicoMachineTemplate is the Schema for the nicomachinetemplates API.
type NicoMachineTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec NicoMachineTemplateSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// NicoMachineTemplateList contains a list of NicoMachineTemplate.
type NicoMachineTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NicoMachineTemplate `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &NicoMachineTemplate{}, &NicoMachineTemplateList{})
}
