// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "k8s.io/apimachinery/pkg/types"

// ResourceName is a Kubernetes DNS subdomain name, not a provider identifier.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=253
// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
type ResourceName string

// QualifiedName is a Kubernetes label or taint key, with an optional DNS prefix.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=317
// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$`
// +kubebuilder:validation:XValidation:rule="self.split('/').size() == 1 ? self.size() <= 63 : self.split('/')[0].size() <= 253 && self.split('/')[1].size() <= 63",message="key name must be at most 63 characters and its DNS prefix at most 253"
type QualifiedName string

// ReleaseReference identifies an operator-published release and its version.
// Resolution and compatibility checks belong to the configuration reconciler.
type ReleaseReference struct {
	Name ResourceName `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Version string `json:"version"`
}

// ClusterReference pins a NodePool to one NKECluster in the same namespace.
// Neither the namespace nor the resource kind is caller-selectable.
type ClusterReference struct {
	Name ResourceName `json:"name"`
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	UID types.UID `json:"uid"`
}

const (
	// ReadyCondition reports whether the resource provides its requested service.
	ReadyCondition = "Ready"
	// ReconcilingCondition reports progress toward the current desired state.
	ReconcilingCondition = "Reconciling"
	// StalledCondition reports a failure requiring a spec change or intervention.
	StalledCondition = "Stalled"
)
