// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Release is one Kubernetes + core-runtime recipe release (docs/PROPOSAL-API-FIELDS.md, "The
// catalog"): the two recipe classes that move together per cluster. metadata.name is the release
// name (v1.34.1-cr2.1.0). It carries the offering envelope (deprecated, hidden) and no status: a
// Release is a declaration, and nothing is observed about it. The content-addressed identity
// (artifact{ref, digest}) and the Verified condition land together once the NKD SRD fixes the
// release-set artifact format.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=releases,shortName=rel,scope=Cluster
// +kubebuilder:printcolumn:name="K8S",type=string,JSONPath=`.spec.kubernetes.version`
// +kubebuilder:printcolumn:name="CORE-RUNTIME",type=string,JSONPath=`.spec.coreRuntime.version`
// +kubebuilder:printcolumn:name="DEPRECATED",type=boolean,JSONPath=`.spec.deprecated`
// +kubebuilder:printcolumn:name="HIDDEN",type=boolean,JSONPath=`.spec.hidden`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
type Release struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the release content and its offering envelope. The content (kubernetes,
	// coreRuntime) is immutable: operators publish a new Release rather than editing one
	// (NKE-DS9-004). The envelope flags deprecated and hidden are the operator's levers.
	// +kubebuilder:validation:Required
	// +nke:external
	Spec ReleaseSpec `json:"spec"`
}

// ReleaseList is a list of Releases.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Release `json:"items"`
}

// ReleaseSpec holds the Kubernetes recipe, the core-runtime recipe and the offering envelope.
type ReleaseSpec struct {
	// Kubernetes is the Kubernetes recipe release this Release ships; immutable.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="kubernetes is immutable"
	// +nke:external
	Kubernetes ReleaseKubernetes `json:"kubernetes"`

	// CoreRuntime is the core-runtime recipe release this Release ships (NKE-DS9-007); immutable.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="coreRuntime is immutable"
	// +nke:external
	CoreRuntime CoreRuntime `json:"coreRuntime"`

	// Deprecated marks the Release as still usable but superseded (UC-002): it is surfaced in
	// conditions (ReleaseCurrent reason UpgradeRequired on clusters holding it) and in the
	// DEPRECATED column, leaves status.release.available for new pins, and never blocks.
	// +kubebuilder:validation:Optional
	// +nke:external
	Deprecated bool `json:"deprecated,omitempty"`

	// Hidden takes the Release off the menu (UC-002): omitted from list and from
	// status.release.available, refused as a new reference; get by name still succeeds and
	// clusters pinned to it are unaffected.
	// +kubebuilder:validation:Optional
	// +nke:external
	Hidden bool `json:"hidden,omitempty"`
}

// ReleaseKubernetes is the Kubernetes recipe release of a Release.
type ReleaseKubernetes struct {
	// Version is the CNCF-conformant upstream Kubernetes version of the control plane and
	// kubelet, e.g. v1.34.1.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`
	// +nke:external
	Version string `json:"version"`
}

// CoreRuntime is the core-runtime recipe release of a Release: its version and the component
// versions it pins (NKE-DS9-007).
type CoreRuntime struct {
	// Version is the core-runtime recipe release, e.g. 2.1.0.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +nke:external
	Version string `json:"version"`

	// Components are the core-runtime components and their versions, at most one per name.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	// +nke:external
	Components []CoreRuntimeComponent `json:"components,omitempty"`
}

// CoreRuntimeComponentName names a core-runtime component; the set is the one NKE-DS9-007
// enumerates.
//
// +kubebuilder:validation:Enum=dns;nke-agent;spire-agent;cni;cri;csi
type CoreRuntimeComponentName string

// The core-runtime components a Release pins.
const (
	CoreRuntimeComponentDNS        CoreRuntimeComponentName = "dns"
	CoreRuntimeComponentNKEAgent   CoreRuntimeComponentName = "nke-agent"
	CoreRuntimeComponentSPIREAgent CoreRuntimeComponentName = "spire-agent"
	CoreRuntimeComponentCNI        CoreRuntimeComponentName = "cni"
	CoreRuntimeComponentCRI        CoreRuntimeComponentName = "cri"
	CoreRuntimeComponentCSI        CoreRuntimeComponentName = "csi"
)

// CoreRuntimeComponent pins one core-runtime component version.
type CoreRuntimeComponent struct {
	// Name identifies the component: dns, nke-agent, spire-agent, cni, cri or csi.
	// +kubebuilder:validation:Required
	// +nke:external
	Name CoreRuntimeComponentName `json:"name"`

	// Version is the component version.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +nke:external
	Version string `json:"version"`
}
