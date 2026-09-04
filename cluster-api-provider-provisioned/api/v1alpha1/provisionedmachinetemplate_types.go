// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ProvisionedMachineTemplateResource holds the machine that Cluster API
// renders from this template.
type ProvisionedMachineTemplateResource struct {
	// ObjectMeta is copied to the rendered machine. Only labels and annotations
	// are used.
	// +optional
	ObjectMeta metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the spec of the rendered ProvisionedMachine.
	Spec ProvisionedMachineSpec `json:"spec"`
}

// ProvisionedMachineTemplateSpec defines the desired state of
// ProvisionedMachineTemplate.
type ProvisionedMachineTemplateSpec struct {
	// Template is the machine to render.
	Template ProvisionedMachineTemplateResource `json:"template"`
}

// +kubebuilder:metadata:labels=cluster.x-k8s.io/provider=infrastructure-provisioned
// +kubebuilder:metadata:labels=cluster.x-k8s.io/v1beta2=v1alpha1
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories=cluster-api,shortName=pmt
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ProvisionedMachineTemplate is the Schema for the
// provisionedmachinetemplates API.
//
// A caller sends this kind as the infra.machineTemplate of a node pool or of
// the cluster control plane configuration. The template usually carries an
// empty spec, because the provider creates nothing: every input it needs is in
// the inventory of the ProvisionedCluster.
type ProvisionedMachineTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ProvisionedMachineTemplateSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// ProvisionedMachineTemplateList contains a list of
// ProvisionedMachineTemplate.
type ProvisionedMachineTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProvisionedMachineTemplate `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &ProvisionedMachineTemplate{}, &ProvisionedMachineTemplateList{})
}

var (
	_ runtime.Object = &ProvisionedMachineTemplate{}
	_ runtime.Object = &ProvisionedMachineTemplateList{}
)
