// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// NicoClusterTemplateSpec defines the desired state of NicoClusterTemplate.
type NicoClusterTemplateSpec struct {
	Template NicoClusterTemplateResource `json:"template"`
}

// NicoClusterTemplateResource describes the data needed to create a NicoCluster from a template.
type NicoClusterTemplateResource struct {
	// Standard object's metadata.
	// +optional
	ObjectMeta clusterv1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	Spec NicoClusterSpec `json:"spec"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories=cluster-api,shortName=nicoct

// NicoClusterTemplate is the Schema for the nicoclustertemplates API.
type NicoClusterTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec NicoClusterTemplateSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// NicoClusterTemplateList contains a list of NicoClusterTemplate.
type NicoClusterTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NicoClusterTemplate `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &NicoClusterTemplate{}, &NicoClusterTemplateList{})
}
