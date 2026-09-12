// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NKECluster is a managed Kubernetes cluster as its tenant sees it (UC-013).
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=nkeclusters,shortName=nkec,scope=Namespaced
// +kubebuilder:printcolumn:name="VERSION",type=string,JSONPath=`.status.kubernetesVersion`
// +kubebuilder:printcolumn:name="READY",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
type NKECluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the tenant's intent.
	// +kubebuilder:validation:Required
	// +nke:tenant=rw
	// +nke:operator=r
	Spec NKEClusterSpec `json:"spec"`

	// Status is what the controller last observed.
	// +kubebuilder:validation:Optional
	// +nke:tenant=r
	// +nke:operator=r
	Status NKEClusterStatus `json:"status,omitempty"`
}

// NKEClusterList is a list of NKEClusters.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type NKEClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NKECluster `json:"items"`
}

// NKEClusterSpec holds the create-only address ranges. The version lever, the API server access
// policy and the accelerated-runtime pin are placeholders below.
//
// +kubebuilder:validation:XValidation:rule="has(self.networking) == has(oldSelf.networking)",message="networking may only be set at creation"
type NKEClusterSpec struct {
	// PLACEHOLDER (UC-002, UC-005, UC-016, NKE-DS9-004, NKE-DS9-006): the version lever.
	// The Kubernetes version is not a field of its own. It arrives with the Release the cluster
	// runs, selected by following a ReleaseChannel and optionally pinning a Release within it:
	//
	//   Release ReleaseSubscription `json:"release"`   // {channel string; name string}
	//
	// Today NKE takes controlPlaneKubernetesVersion (X.Y.Z) directly and has no release or
	// channel concept. Revisit with the Release and ReleaseChannel kinds.

	// Networking fixes the cluster's address ranges at creation.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="networking is immutable"
	// +nke:tenant=rw
	// +nke:operator=r
	Networking *NKEClusterNetworking `json:"networking,omitempty"`

	// PLACEHOLDER (NKE-DS9-014): who may reach the cluster's API server.
	//
	//   APIServerAccess *APIServerAccess `json:"apiServerAccess,omitempty"`   // {authorizedNetworks []CIDR}
	//
	// The AKS apiServerAccessProfile shape. Not implemented in NKE; revisit with the
	// control-plane load-balancer design (machine-provider requirement 22).

	// PLACEHOLDER (UC-018, NKE-DS9-008, NKE-DS9-011): the optional accelerated-runtime pin.
	//
	//   AcceleratedRuntime *AcceleratedRuntimeSelection `json:"acceleratedRuntime,omitempty"`   // {name, version}
	//
	// P0 is manual install with helm; the API form is P1. Not implemented in NKE.
	//
	// Control-plane management (UC-009) is operator-only and "fully managed/hidden from the
	// customer tenants", so it is not a field of this kind at all; it belongs to an operator-owned
	// sibling kind (see doc.go, "Operator intent").
}

// NKEClusterNetworking holds the pod and service CIDRs. NKE today accepts a comma-separated
// list of pod CIDRs and requires the ovn-kubernetes CNI for non-default values; the CNI is a
// core-runtime recipe choice (NKE-DS9-007), not a tenant field.
type NKEClusterNetworking struct {
	// PodCIDR is the range pods are addressed from.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="podCIDR must be a CIDR"
	// +nke:tenant=rw
	// +nke:operator=r
	PodCIDR string `json:"podCIDR,omitempty"`

	// ServiceCIDR is the range ClusterIP services are addressed from.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="serviceCIDR must be a CIDR"
	// +nke:tenant=rw
	// +nke:operator=r
	ServiceCIDR string `json:"serviceCIDR,omitempty"`
}

// NKEClusterStatus carries the running version, how to reach the API server, and conditions.
type NKEClusterStatus struct {
	// ObservedGeneration is the metadata.generation this status reflects.
	// +kubebuilder:validation:Optional
	// +nke:tenant=r
	// +nke:operator=r
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// KubernetesVersion is the version the control plane is running.
	// +kubebuilder:validation:Optional
	// +nke:tenant=r
	// +nke:operator=r
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`

	// PLACEHOLDER (UC-002, UC-016): the release position, once spec.release exists.
	//
	//   Release *ReleaseSubscriptionStatus `json:"release,omitempty"`   // {channel, current, offered, available []string}

	// Endpoint is the URL of the cluster's API server (UC-014).
	// +kubebuilder:validation:Optional
	// +nke:tenant=r
	// +nke:operator=r
	Endpoint string `json:"endpoint,omitempty"`

	// CertificateAuthorityData is the base64-encoded PEM CA bundle for Endpoint (UC-014). With
	// Endpoint it is the non-secret half of a kubeconfig, the EKS certificateAuthority.data and
	// GKE masterAuth.clusterCaCertificate shape; the credential comes from the OIDC issuer, not
	// from this API.
	// +kubebuilder:validation:Optional
	// +nke:tenant=r
	// +nke:operator=r
	CertificateAuthorityData string `json:"certificateAuthorityData,omitempty"`

	// PLACEHOLDER (UC-014, NKE-DS9-003): what a client needs to build the kubeconfig exec stanza
	// for OIDC against Endpoint.
	//
	//   OIDC *OIDCAccess `json:"oidc,omitempty"`   // {issuerURL, clientID}
	//
	// NKE already wires an OIDC issuer into the tenant kube-apiserver (enableOidc); the values are
	// operator configuration. Revisit with the identity design.

	// PLACEHOLDER (UC-018, NKE-DS9-011): the accelerated-runtime bundle actually installed.
	//
	//   AcceleratedRuntime *AcceleratedRuntimeStatus `json:"acceleratedRuntime,omitempty"`   // {name, version, state}

	// Conditions are Ready (reasons Serving, Provisioning, Deleting) and Progressing (reasons
	// Idle, Provisioning, Upgrading, Deleting, Blocked).
	//
	// PLACEHOLDER conditions, added with their features: ReleaseCurrent (UC-016; reasons Current,
	// UpgradeAvailable, UpgradeRequired) and the readiness validation conditions Validated,
	// NetworkValidated, AcceleratorValidated (NKE-DS9-012; see validation.go).
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +nke:tenant=r
	// +nke:operator=r
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// There is no controller-only state on a cluster. The endpoint, CA and version are read from
	// the backend on every reconcile and reported above; the release the cluster is observed
	// running (UC-004) will be status.release.current, readable by both roles.
}
