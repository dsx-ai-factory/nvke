// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NKECluster is a managed Kubernetes cluster as its tenant sees it.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=nkeclusters,shortName=nkec,scope=Namespaced
// +kubebuilder:printcolumn:name="CHANNEL",type=string,JSONPath=`.spec.release.channel`
// +kubebuilder:printcolumn:name="CURRENT",type=string,JSONPath=`.status.release.current`
// +kubebuilder:printcolumn:name="VERSION",type=string,JSONPath=`.status.kubernetesVersion`
// +kubebuilder:printcolumn:name="ACCEL-RUNTIME",type=string,JSONPath=`.status.acceleratedRuntime.name`
// +kubebuilder:printcolumn:name="VALIDATED",type=string,JSONPath=`.status.conditions[?(@.type=="Validated")].status`
// +kubebuilder:printcolumn:name="READY",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
type NKECluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the tenant's intent.
	// +kubebuilder:validation:Required
	// +nke:external
	Spec NKEClusterSpec `json:"spec"`

	// Status is what the controller last observed.
	// +kubebuilder:validation:Optional
	// +nke:external
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

// NKEClusterSpec holds the release subscription, the create-only address ranges, the API server
// access policy and the optional accelerated-runtime pin.
//
// +kubebuilder:validation:XValidation:rule="has(self.networking) == has(oldSelf.networking)",message="networking may only be set at creation"
type NKEClusterSpec struct {
	// Release is the subscription: the ReleaseChannel the cluster follows and, optionally, the
	// Release it holds or targets within that channel.
	// +kubebuilder:validation:Required
	// +nke:external
	Release ReleaseSubscription `json:"release"`

	// Networking fixes the cluster's address ranges at creation.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="networking is immutable"
	// +nke:external
	Networking *NKEClusterNetworking `json:"networking,omitempty"`

	// APIServerAccess restricts who can reach the cluster's API server (NKE-DS9-014). Written by
	// the tenant; applied by the machine-provider adapter to the control-plane load balancer.
	// +kubebuilder:validation:Optional
	// +nke:external
	APIServerAccess *APIServerAccess `json:"apiServerAccess,omitempty"`

	// AcceleratedRuntime pins the optional accelerated-runtime bundle (GPU and network
	// operators, drivers) the cluster runs; changing version is an upgrade or a rollback
	// (NKE-DS9-011). Format-validated and resolved against the AcceleratedRuntime kind. Written by
	// the tenant; acted on by the accelerated-runtime controller.
	// +kubebuilder:validation:Optional
	// +nke:external
	AcceleratedRuntime *AcceleratedRuntimeSelection `json:"acceleratedRuntime,omitempty"`
}

// ReleaseSubscription is the version model's subscription (docs/PROPOSAL-API-FIELDS.md, "The
// version model"): the channel decides which Releases exist and whether the cluster moves
// automatically; the optional name selects within the channel.
type ReleaseSubscription struct {
	// Channel is the metadata.name of the ReleaseChannel to follow. Mutable, so a cluster can
	// move between channels; a switch is admitted only if the current Release is available in
	// the target channel.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +nke:external
	Channel string `json:"channel"`

	// Name is the Release the cluster holds or targets, one of status.release.available; unset,
	// the cluster follows the channel head. Set ahead of status.release.current it is a
	// tenant-initiated upgrade; left behind the head it is a hold.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=253
	// +nke:external
	Name string `json:"name,omitempty"`
}

// NKEClusterNetworking holds the pod and service CIDRs.
type NKEClusterNetworking struct {
	// PodCIDR is the range pods are addressed from.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="podCIDR must be a CIDR"
	// +nke:external
	PodCIDR string `json:"podCIDR,omitempty"`

	// ServiceCIDR is the range ClusterIP services are addressed from.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="serviceCIDR must be a CIDR"
	// +nke:external
	ServiceCIDR string `json:"serviceCIDR,omitempty"`
}

// APIServerAccess is the cluster's API server access policy (the AKS apiServerAccessProfile
// shape).
type APIServerAccess struct {
	// AuthorizedNetworks are the CIDRs permitted to reach the API server; empty means the
	// cluster's VPC only. Applied by the machine-provider adapter to the control-plane load
	// balancer.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=40
	// +kubebuilder:validation:items:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="self.all(c, isCIDR(c))",message="authorizedNetworks must be CIDRs"
	// +listType=set
	// +nke:external
	AuthorizedNetworks []string `json:"authorizedNetworks,omitempty"`
}

// AcceleratedRuntimeSelection is the tenant's accelerated-runtime pin: a catalog selection by
// concept name (docs/PROPOSAL-API-FIELDS.md, "Naming"), not a reference.
type AcceleratedRuntimeSelection struct {
	// Name is the accelerated-runtime bundle, e.g. gpu-runtime-3.2.0; the AcceleratedRuntime
	// kind's metadata.name when it lands.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	// +nke:external
	Name string `json:"name"`

	// Version is the bundle version pinned; changing it is an upgrade or a rollback.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=64
	// +nke:external
	Version string `json:"version,omitempty"`
}

// NKEClusterStatus carries the running version, the release position, reachability, the
// accelerated runtime and conditions.
type NKEClusterStatus struct {
	// ObservedGeneration is the metadata.generation this status reflects.
	// +kubebuilder:validation:Optional
	// +nke:external
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// KubernetesVersion is the version the control plane is running.
	// +kubebuilder:validation:Optional
	// +nke:external
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`

	// Release reports the channel followed, the Releases current and offered, and what
	// spec.release.name may name.
	// +kubebuilder:validation:Optional
	// +nke:external
	Release *ReleaseSubscriptionStatus `json:"release,omitempty"`

	// Endpoint is the URL of the cluster's API server.
	// +kubebuilder:validation:Optional
	// +nke:external
	Endpoint string `json:"endpoint,omitempty"`

	// CertificateAuthorityData is the base64-encoded PEM CA bundle for Endpoint: the non-secret
	// half of a kubeconfig.
	// +kubebuilder:validation:Optional
	// +nke:external
	CertificateAuthorityData string `json:"certificateAuthorityData,omitempty"`

	// AcceleratedRuntime is the accelerated-runtime bundle actually installed and its state
	// (NKE-DS9-011). Written by the reconciler from the machine-provider adapter's report.
	// +kubebuilder:validation:Optional
	// +nke:external
	AcceleratedRuntime *AcceleratedRuntimeStatus `json:"acceleratedRuntime,omitempty"`

	// Conditions are Ready (reasons Serving, Provisioning, Deleting, ChannelNotFound), Progressing
	// (reasons Idle, Provisioning, Upgrading, Deleting, Blocked), ReleaseCurrent (reasons Current,
	// UpgradeAvailable, UpgradeRequired; ForcedUpgrade later) and the readiness validation
	// conditions Validated (the aggregate; gates Ready), NetworkValidated and AcceleratorValidated
	// (reasons Pending, Running, Passed, Failed).
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +nke:external
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Internal is controller-only state the tenant never sees.
	// +kubebuilder:validation:Optional
	Internal NKEClusterInternalState `json:"internal,omitempty"`
}

// ReleaseSubscriptionStatus compares where the cluster is (current) with what the channel
// offers (offered: the channel's head) and lists what the tenant may pin (available). The
// cluster is asked to run spec.release.name when pinned, else offered.
type ReleaseSubscriptionStatus struct {
	// Channel is the ReleaseChannel the cluster follows.
	// +kubebuilder:validation:Optional
	// +nke:external
	Channel string `json:"channel,omitempty"`

	// Current is the Release the cluster is running.
	// +kubebuilder:validation:Optional
	// +nke:external
	Current string `json:"current,omitempty"`

	// Offered is the channel's head: the Release the channel offers. A cluster pinned behind it
	// (spec.release.name) keeps running its pin and reads ReleaseCurrent UpgradeAvailable.
	// +kubebuilder:validation:Optional
	// +nke:external
	Offered string `json:"offered,omitempty"`

	// Available are the Releases spec.release.name may name: the head, then the channel's prior
	// offered Releases that are neither deprecated nor hidden, newest first (OpenShift
	// availableUpdates).
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	// +nke:external
	Available []string `json:"available,omitempty"`
}

// AcceleratedRuntimeState is the state of the accelerated-runtime bundle on a cluster.
//
// +kubebuilder:validation:Enum=Installing;Installed;Upgrading;RollingBack;Failed
type AcceleratedRuntimeState string

// The accelerated-runtime states: the pin, upgrade and rollback of NKE-DS9-011, observable.
const (
	AcceleratedRuntimeInstalling  AcceleratedRuntimeState = "Installing"
	AcceleratedRuntimeInstalled   AcceleratedRuntimeState = "Installed"
	AcceleratedRuntimeUpgrading   AcceleratedRuntimeState = "Upgrading"
	AcceleratedRuntimeRollingBack AcceleratedRuntimeState = "RollingBack"
	AcceleratedRuntimeFailed      AcceleratedRuntimeState = "Failed"
)

// AcceleratedRuntimeStatus is the accelerated-runtime bundle as installed.
type AcceleratedRuntimeStatus struct {
	// Name is the installed bundle.
	// +kubebuilder:validation:Optional
	// +nke:external
	Name string `json:"name,omitempty"`

	// Version is the installed bundle version.
	// +kubebuilder:validation:Optional
	// +nke:external
	Version string `json:"version,omitempty"`

	// State is Installing, Installed, Upgrading, RollingBack or Failed.
	// +kubebuilder:validation:Optional
	// +nke:external
	State AcceleratedRuntimeState `json:"state,omitempty"`
}

// NKEClusterInternalState is the controller-only state of a cluster (docs/PROPOSAL-API-FIELDS.md,
// "Hidden fields"). The machine-provider adapter owns the observed* fields and writes them by
// merge patch; the reconciler owns the rest.
type NKEClusterInternalState struct {
	// ObservedRelease is the Release the machine provider reports as running.
	// +kubebuilder:validation:Optional
	ObservedRelease string `json:"observedRelease,omitempty"`

	// ObservedEndpoint is the API server URL the machine provider reports.
	// +kubebuilder:validation:Optional
	ObservedEndpoint string `json:"observedEndpoint,omitempty"`

	// ObservedValidation is the machine provider's readiness report; the reconciler folds it into
	// the Validated, NetworkValidated and AcceleratorValidated conditions.
	// +kubebuilder:validation:Optional
	ObservedValidation *ObservedValidation `json:"observedValidation,omitempty"`

	// ValidatedFor records what the last passing validation ran against, so a release or spec
	// change re-triggers validation instead of leaving Validated: True stale.
	// +kubebuilder:validation:Optional
	ValidatedFor *ValidatedFor `json:"validatedFor,omitempty"`

	// ValidationRequestedAt is when an operator last requested a re-run of readiness validation.
	// +kubebuilder:validation:Optional
	ValidationRequestedAt *metav1.Time `json:"validationRequestedAt,omitempty"`

	// ProviderRefs names the substrate objects behind the cluster, keyed by role.
	// +kubebuilder:validation:Optional
	ProviderRefs map[string]string `json:"providerRefs,omitempty"`
}

// ValidatedFor is the target a passing readiness validation was run against.
type ValidatedFor struct {
	// Release is the Release the cluster ran when validation passed.
	// +kubebuilder:validation:Optional
	Release string `json:"release,omitempty"`

	// Generation is the metadata.generation of the cluster when validation passed.
	// +kubebuilder:validation:Optional
	Generation int64 `json:"generation,omitempty"`
}
