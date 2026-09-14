// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// NicoClusterSpec defines the desired state of NicoCluster.
type NicoClusterSpec struct {
	// IdentityRef points to the Secret with NICo endpoint and authentication settings.
	// When unset, the controller falls back to the provider-level credentials Secret
	// in the manager namespace.
	// +optional
	IdentityRef corev1.LocalObjectReference `json:"identityRef,omitempty,omitzero"`

	// SiteID is the site where this provider should create and look up instances.
	SiteID string `json:"siteID"`

	// FailureDomainLabelKey is the NICo Machine label key that carries the failure
	// domain name. It is read to publish status.failureDomains and sent as the
	// machine label selector key when placing an instance. Leaving it unset
	// disables failure domain support for this cluster: no domains are published,
	// and a Machine that requests one fails rather than being placed anywhere.
	// Sites that follow the NICo convention set it to "failure_domain".
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	FailureDomainLabelKey string `json:"failureDomainLabelKey,omitempty"`
}

// NicoClusterInitializationStatus provides observations of the NicoCluster initialization process.
// +kubebuilder:validation:MinProperties=1
type NicoClusterInitializationStatus struct {
	// Provisioned is true once the provider has validated NICo access for this cluster.
	// +optional
	Provisioned *bool `json:"provisioned,omitempty"`
}

// NicoClusterStatus defines the observed state of NicoCluster.
type NicoClusterStatus struct {
	// Initialization provides provisioning observations for this NICo-backed cluster.
	// +optional
	Initialization NicoClusterInitializationStatus `json:"initialization,omitempty,omitzero"`

	// Conditions describes the current cluster state.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Ready is true once the provider can reach NICo and resolve tenant context for this cluster.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// FailureDomains are the placement domains NICo exposes for this cluster's site.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	FailureDomains []clusterv1.FailureDomain `json:"failureDomains,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories=cluster-api,shortName=nicoc
// +kubebuilder:printcolumn:name="Available",type="string",JSONPath=".status.conditions[?(@.type=='Available')].status",description="Current cluster infrastructure availability"
// +kubebuilder:printcolumn:name="Synced",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status",description="Cluster infrastructure reconciled"
// +kubebuilder:printcolumn:name="Provisioned",type="string",JSONPath=".status.initialization.provisioned",description="Initial cluster infrastructure provisioning completed"
// +kubebuilder:printcolumn:name="Site",type="string",JSONPath=".spec.siteID",description="NICo site"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// NicoCluster is the Schema for the nicoclusters API.
type NicoCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NicoClusterSpec   `json:"spec,omitempty"`
	Status NicoClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NicoClusterList contains a list of NicoCluster.
type NicoClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NicoCluster `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &NicoCluster{}, &NicoClusterList{})
}

// GetConditions returns the set of conditions for this object.
func (c *NicoCluster) GetConditions() []metav1.Condition {
	return c.Status.Conditions
}

// GetCondition returns the condition with the given type, or Unknown when it
// has not been reported.
func (c *NicoCluster) GetCondition(conditionType string) metav1.Condition {
	condition := apimeta.FindStatusCondition(c.Status.Conditions, conditionType)
	if condition == nil {
		return metav1.Condition{Type: conditionType, Status: metav1.ConditionUnknown, Reason: UnknownReason, Message: UnknownReason}
	}
	return *condition
}

// SetConditions sets conditions for this object.
func (c *NicoCluster) SetConditions(conditions []metav1.Condition) {
	c.Status.Conditions = conditions
}
