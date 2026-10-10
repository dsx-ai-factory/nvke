// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ClusterReleaseChannelSpec lists the releases available through a channel.
type ClusterReleaseChannelSpec struct {
	// Releases maps Kubernetes major.minor versions to their available releases.
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:XValidation:rule="self.all(version, version.matches('^(0|[1-9][0-9]*)[.](0|[1-9][0-9]*)$'))",message="release keys must be Kubernetes major.minor versions"
	Releases map[string]ClusterReleaseReferences `json:"releases"`
}

// ClusterReleaseReferences lists the releases available for one Kubernetes minor version.
// +kubebuilder:validation:MinItems=1
// +listType=map
// +listMapKey=name
type ClusterReleaseReferences []ClusterReleaseReference

// ClusterReleaseReference names a cluster-scoped ClusterRelease.
type ClusterReleaseReference struct {
	Name ResourceName `json:"name"`
	// MinimumUpgradeAge is the minimum time since this release was created before it is eligible for automatic adoption.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="minimumUpgradeAge must be a nonnegative duration"
	MinimumUpgradeAge metav1.Duration `json:"minimumUpgradeAge"`
}

// ClusterReleaseChannel is an operator-managed catalog of control-plane releases.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,path=clusterreleasechannels,singular=clusterreleasechannel
// +kubebuilder:storageversion
type ClusterReleaseChannel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ClusterReleaseChannelSpec `json:"spec"`
}

// ClusterReleaseChannelList is a list of release channels.
// +kubebuilder:object:root=true
type ClusterReleaseChannelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterReleaseChannel `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClusterReleaseChannel{}, &ClusterReleaseChannelList{})
		return nil
	})
}
