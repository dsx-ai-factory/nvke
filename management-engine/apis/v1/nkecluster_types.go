// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// NKEClusterSpec describes tenant intent for a managed control plane.
// +kubebuilder:validation:XValidation:rule="has(self.authentication) == has(oldSelf.authentication) && (!has(self.authentication) || self.authentication == oldSelf.authentication)",message="authentication is only configurable at cluster creation"
type NKEClusterSpec struct {
	APIServerAccess APIServerAccess `json:"apiServerAccess"`
	// Authentication configures additional tenant JWT issuers. The engine adds
	// the platform issuer separately; omission does not disable platform identity.
	// +optional
	Authentication    *ClusterAuthentication  `json:"authentication,omitempty"`
	ClusterNetwork    ClusterNetwork          `json:"clusterNetwork"`
	Release           ClusterReleaseSelection `json:"release"`
	ControlPlaneClass ResourceName            `json:"controlPlaneClass"`
	// AcceleratedRuntime selects a runtime bundle; name "none" requests opt-out,
	// subject to operator policy. Omission leaves selection to engine policy.
	// +optional
	AcceleratedRuntime *AcceleratedRuntimeSelection `json:"acceleratedRuntime,omitempty"`
}

// APIServerAccess describes the requested network access to the workload API.
type APIServerAccess struct {
	PublicAccessEnabled bool `json:"publicAccessEnabled"`
	// +optional
	// +listType=map
	// +listMapKey=cidr
	// +kubebuilder:validation:MaxItems=64
	PublicAccessCIDRs []PublicAccessCIDR `json:"publicAccessCidrs,omitempty"`
}

// PublicAccessCIDR is an allowed source network and optional description.
type PublicAccessCIDR struct {
	// +kubebuilder:validation:MaxLength=49
	// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="must be an IPv4 or IPv6 CIDR"
	CIDR string `json:"cidr"`
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Description string `json:"description,omitempty"`
}

// ClusterNetwork describes the pod/service networks and cluster DNS domain.
type ClusterNetwork struct {
	Pods          NetworkRanges `json:"pods"`
	Services      NetworkRanges `json:"services"`
	ServiceDomain ResourceName  `json:"serviceDomain"`
}

// NetworkRanges contains the CIDRs requested for a cluster network.
type NetworkRanges struct {
	// Order is significant: the first CIDR selects the default IP family. A
	// second CIDR makes the network dual-stack, which Kubernetes accepts only
	// as one range per family.
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:XValidation:rule="self.size() < 2 || self[0] != self[1]",message="CIDRs must be distinct"
	// +kubebuilder:validation:XValidation:rule="self.size() < 2 || cidr(self[0]).ip().family() != cidr(self[1]).ip().family()",message="CIDRs must be of different IP families"
	CIDRBlocks []NetworkCIDR `json:"cidrBlocks"`
}

// NetworkCIDR is an IPv4 or IPv6 network in CIDR notation.
// +kubebuilder:validation:MaxLength=49
// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="must be an IPv4 or IPv6 CIDR"
type NetworkCIDR string

// ClusterReleaseSelection selects the release channel used for the cluster.
type ClusterReleaseSelection struct {
	Channel ReleaseChannelReference `json:"channel"`
}

// ReleaseChannelReference identifies a channel and an optional preferred version.
// Version may be a minor version or an exact release, as permitted by the channel.
type ReleaseChannelReference struct {
	Name ResourceName `json:"name"`
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Version string `json:"version,omitempty"`
}

// AcceleratedRuntimeSelection selects a runtime release or explicitly opts out.
// +kubebuilder:validation:XValidation:rule="self.name == 'none' ? !has(self.version) : has(self.version)",message="version is required for a runtime release and must be omitted for none"
type AcceleratedRuntimeSelection struct {
	Name ResourceName `json:"name"`
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Version string `json:"version,omitempty"`
}

// NKEClusterStatus reports observed state; only controllers write status.
type NKEClusterStatus struct {
	// +optional
	// +kubebuilder:validation:Minimum=0
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions use positive polarity for Ready and report the generation each
	// observation describes. Absence means unknown, not false.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ResolvedRelease is the actual selected control-plane release, which can
	// differ from the tenant's channel preference after an operator override.
	// +optional
	ResolvedRelease *ReleaseReference `json:"resolvedRelease,omitempty"`
}

// NKECluster is the internal representation of a managed workload control plane.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,path=nkeclusters,singular=nkecluster
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type NKECluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NKEClusterSpec `json:"spec"`
	// +optional
	Status NKEClusterStatus `json:"status,omitempty,omitzero"`
}

// NKEClusterList is a list of internal managed clusters.
// +kubebuilder:object:root=true
type NKEClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NKECluster `json:"items"`
}
