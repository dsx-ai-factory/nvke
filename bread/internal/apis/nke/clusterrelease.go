// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterRelease is the release assignment of one NKECluster; created only by the controller.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=clusterreleases,shortName=crel,scope=Namespaced
// +kubebuilder:printcolumn:name="CLUSTER",type=string,JSONPath=`.spec.clusterName`
// +kubebuilder:printcolumn:name="CHANNEL",type=string,JSONPath=`.spec.channel`
// +kubebuilder:printcolumn:name="RELEASE",type=string,JSONPath=`.spec.releaseName`
// +kubebuilder:printcolumn:name="CURRENT",type=string,JSONPath=`.status.current`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
type ClusterRelease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the assignment.
	// +kubebuilder:validation:Required
	// +nke:external
	Spec ClusterReleaseSpec `json:"spec"`

	// Status is what the controller last observed.
	// +kubebuilder:validation:Optional
	// +nke:external
	Status ClusterReleaseStatus `json:"status,omitempty"`
}

// ClusterReleaseList is a list of ClusterReleases.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ClusterReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterRelease `json:"items"`
}

// ClusterReleaseSpec holds the cluster, the release it is being moved to and the channel that selected it.
type ClusterReleaseSpec struct {
	// ClusterName is the metadata.name of the NKECluster this assignment belongs to, resolved in
	// the same namespace as the ClusterRelease; immutable. The field keeps the name clusterName
	// (it names an NKECluster). Held to the NKECluster's own name rule (a DNS-1123 label of at
	// most 55 characters).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=55
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterName is immutable"
	// +nke:external
	ClusterName string `json:"clusterName"`

	// ReleaseName is the metadata.name of the Release the cluster is being moved to; Releases
	// are cluster-scoped, so the name alone resolves it.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +nke:external
	ReleaseName string `json:"releaseName"`

	// Channel is the ReleaseChannel that selected ReleaseName; empty when the release is pinned.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=253
	// +nke:external
	Channel string `json:"channel,omitempty"`
}

// ClusterReleaseStatus carries the current and offered releases and conditions.
type ClusterReleaseStatus struct {
	// ObservedGeneration is the metadata.generation this status reflects.
	// +kubebuilder:validation:Optional
	// +nke:external
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Current is the Release the cluster is running.
	// +kubebuilder:validation:Optional
	// +nke:external
	Current string `json:"current,omitempty"`

	// Offered is the Release the channel offers, its head. What the cluster is asked to run is
	// spec.releaseName: the pin when set, else this.
	// +kubebuilder:validation:Optional
	// +nke:external
	Offered string `json:"offered,omitempty"`

	// Conditions are Ready, Progressing and ReleaseCurrent.
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +nke:external
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}
