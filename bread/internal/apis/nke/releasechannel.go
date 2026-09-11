// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReleaseChannel offers one Release to the NKEClusters that follow it; its promotion history is
// where a cluster's status.release.available comes from (docs/PROPOSAL-API-FIELDS.md, "The
// version model"). Paced rollout (cohorts) is later.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=releasechannels,shortName=chan,scope=Cluster
// +kubebuilder:printcolumn:name="RELEASE",type=string,JSONPath=`.spec.releaseName`
// +kubebuilder:printcolumn:name="READY",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
type ReleaseChannel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the operator's intent.
	// +kubebuilder:validation:Required
	// +nke:external
	Spec ReleaseChannelSpec `json:"spec"`

	// Status is what the controller last observed.
	// +kubebuilder:validation:Optional
	// +nke:external
	Status ReleaseChannelStatus `json:"status,omitempty"`
}

// ReleaseChannelList is a list of ReleaseChannels.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ReleaseChannelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ReleaseChannel `json:"items"`
}

// ReleaseChannelSpec holds the offered release.
type ReleaseChannelSpec struct {
	// ReleaseName is the metadata.name of the Release the channel offers (its head); Releases
	// are cluster-scoped, so the name alone resolves it. Clusters following the channel move to
	// it unless they pin a Release in status.release.available.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +nke:external
	ReleaseName string `json:"releaseName"`
}

// ReleaseChannelStatus carries the promotion history and conditions.
type ReleaseChannelStatus struct {
	// ObservedGeneration is the metadata.generation this status reflects.
	// +kubebuilder:validation:Optional
	// +nke:external
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// History records every release the channel has offered, oldest first. It is the source of
	// every following cluster's status.release.available.
	// +kubebuilder:validation:Optional
	// +listType=atomic
	// +nke:external
	History []Promotion `json:"history,omitempty"`

	// Conditions are Ready: True with reason ReleaseFound when the offered Release exists (reason
	// Deprecated when it exists and is deprecated), False with reason ReleaseNotFound otherwise.
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +nke:external
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// Promotion records one release the channel started offering, and when.
type Promotion struct {
	// ReleaseName is the metadata.name of the Release that was offered.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +nke:external
	ReleaseName string `json:"releaseName"`

	// PromotedAt is when the channel started offering it.
	// +kubebuilder:validation:Required
	// +nke:external
	PromotedAt metav1.Time `json:"promotedAt"`
}
