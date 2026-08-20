// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// PreprovisionedMachineTemplateResource holds the machine that Cluster API
// renders from this template.
type PreprovisionedMachineTemplateResource struct {
	// ObjectMeta is copied to the rendered machine. Only labels and annotations
	// are used.
	// +optional
	ObjectMeta metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the spec of the rendered PreprovisionedMachine.
	Spec PreprovisionedMachineSpec `json:"spec"`
}

// PreprovisionedMachineTemplateSpec defines the desired state of
// PreprovisionedMachineTemplate.
type PreprovisionedMachineTemplateSpec struct {
	// Template is the machine to render.
	Template PreprovisionedMachineTemplateResource `json:"template"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories=cluster-api,shortName=ppmt
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// PreprovisionedMachineTemplate is the Schema for the
// preprovisionedmachinetemplates API.
//
// A caller sends this kind as the capiInfraMachineTemplate of a node pool or of
// the cluster control plane configuration. The template usually carries an
// empty spec, because the provider creates nothing: every input it needs is in
// the inventory of the PreprovisionedCluster.
type PreprovisionedMachineTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PreprovisionedMachineTemplateSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// PreprovisionedMachineTemplateList contains a list of
// PreprovisionedMachineTemplate.
type PreprovisionedMachineTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PreprovisionedMachineTemplate `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &PreprovisionedMachineTemplate{}, &PreprovisionedMachineTemplateList{})
}

var (
	_ runtime.Object = &PreprovisionedMachineTemplate{}
	_ runtime.Object = &PreprovisionedMachineTemplateList{}
)
