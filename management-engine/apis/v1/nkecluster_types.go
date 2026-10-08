// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// NKEClusterSpec configures a cluster's control plane and node pools.
// +kubebuilder:validation:XValidation:rule="has(self.authentication) == has(oldSelf.authentication) && (!has(self.authentication) || self.authentication == oldSelf.authentication)",message="authentication is only configurable at cluster creation"
// +kubebuilder:validation:XValidation:rule="!has(self.nodePools) || self.nodePools.size() == 0 || has(self.network)",message="network is required when nodePools are configured"
type NKEClusterSpec struct {
	APIServerAccess APIServerAccess `json:"apiServerAccess"`
	// Authentication configures additional JWT issuers for cluster access.
	// It can only be set when creating the cluster.
	// +optional
	Authentication *ClusterAuthentication `json:"authentication,omitempty"`
	ClusterNetwork ClusterNetwork         `json:"clusterNetwork"`
	Release        ReleaseSelection       `json:"release"`
	// ControlPlaneClass names the class that determines control-plane sizing and configuration.
	ControlPlaneClass ResourceName `json:"controlPlaneClass"`
	// AcceleratedRuntime selects an accelerated runtime bundle. Use name "none" to opt out.
	// +optional
	AcceleratedRuntime *AcceleratedRuntimeSelection `json:"acceleratedRuntime,omitempty"`
	// Network selects the existing VPC shared by the node pools.
	// +optional
	Network *Network `json:"network,omitempty"`
	// NodePools configures groups of nodes in the cluster. Omission requests
	// a control plane without worker nodes. Names identify pools across updates.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	NodePools []NodePool `json:"nodePools,omitempty"`
}

// Network identifies the existing VPC used by the node pools.
type Network struct {
	// VPCID is the ID of the VPC in which node pools are provisioned. The VPC must already exist.
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:MaxLength=36
	VPCID string `json:"vpcID"`
}

// APIServerAccess controls access to the cluster's Kubernetes API server from outside the VPC.
type APIServerAccess struct {
	// PublicAccessEnabled enables public access to the Kubernetes API server.
	PublicAccessEnabled bool `json:"publicAccessEnabled"`
	// PublicAccessCIDRs lists the source networks allowed to use public API server access.
	// +optional
	// +listType=map
	// +listMapKey=cidr
	// +kubebuilder:validation:MaxItems=64
	PublicAccessCIDRs []PublicAccessCIDR `json:"publicAccessCidrs,omitempty"`
}

// PublicAccessCIDR is an allowed source network and optional description.
type PublicAccessCIDR struct {
	// CIDR is an IPv4 or IPv6 source network in CIDR notation.
	// +kubebuilder:validation:MaxLength=49
	// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="must be an IPv4 or IPv6 CIDR"
	CIDR string `json:"cidr"`
	// Description is an optional label for the source network.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Description string `json:"description,omitempty"`
}

// ClusterNetwork describes the pod/service networks and cluster DNS domain.
type ClusterNetwork struct {
	// Pods specifies the address ranges used by pods.
	Pods NetworkRanges `json:"pods"`
	// Services specifies the address ranges used by cluster services.
	Services NetworkRanges `json:"services"`
	// ServiceDomain is the DNS domain used by cluster services.
	ServiceDomain ResourceName `json:"serviceDomain"`
}

// NetworkRanges contains the CIDRs requested for a cluster network.
type NetworkRanges struct {
	// CIDRBlocks contains one address range, or one IPv4 and one IPv6 range for dual-stack.
	// The first range selects the default IP family.
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

// ReleaseSelection selects the release channel used for the control plane.
type ReleaseSelection struct {
	Channel ReleaseChannelReference `json:"channel"`
}

// ReleaseChannelReference selects a release channel and an optional version within it.
type ReleaseChannelReference struct {
	// Name identifies the release channel.
	Name ResourceName `json:"name"`
	// Version optionally selects a version within the release channel.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Version string `json:"version,omitempty"`
}

// AcceleratedRuntimeSelection selects a runtime release or explicitly opts out.
// +kubebuilder:validation:XValidation:rule="self.name == 'none' ? !has(self.version) : has(self.version)",message="version is required for a runtime release and must be omitted for none"
type AcceleratedRuntimeSelection struct {
	Name ResourceName `json:"name"`
	// Version is an OCI tag. It need not be a semantic version.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`
	Version string `json:"version,omitempty"`
}

// NKEClusterStatus reports the observed state of the control plane and node pools.
type NKEClusterStatus struct {
	// ObservedGeneration identifies the cluster generation reflected in this status.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Provisioning preserves creation identity across retries and spec changes.
	// +optional
	Provisioning *ClusterProvisioningStatus `json:"provisioning,omitempty"`
	// Conditions report readiness and progress, with details in reason and message.
	// A missing condition means its state is unknown.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ResolvedRelease identifies the control-plane release selected for this cluster.
	// +optional
	ResolvedRelease *ReleaseReference `json:"resolvedRelease,omitempty"`
	// NodePools reports the observed state of each pool, identified by its name in spec.nodePools.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	NodePools []NodePoolStatus `json:"nodePools,omitempty"`
}

// ClusterProvisioningStatus identifies the creation attempt independently of the latest observed spec.
type ClusterProvisioningStatus struct {
	// Endpoint pins the service so configuration changes cannot redirect an existing cluster.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Endpoint string `json:"endpoint"`
	// ID identifies the created cluster. It is absent until creation is confirmed.
	// +optional
	// +kubebuilder:validation:Format=uuid
	ID string `json:"id,omitempty"`
	// Generation records the spec used to initiate creation, not merely observed by reconciliation.
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
}

// NKECluster manages a Kubernetes control plane and its node pools as one cluster.
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

// NKEClusterList is a list of managed Kubernetes clusters.
// +kubebuilder:object:root=true
type NKEClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NKECluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &NKECluster{}, &NKEClusterList{})
		return nil
	})
}

func (c *NKECluster) GetConditions() []metav1.Condition {
	return c.Status.Conditions
}

func (c *NKECluster) SetConditions(conditions []metav1.Condition) {
	c.Status.Conditions = conditions
}
