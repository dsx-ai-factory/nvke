// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cluster classes define operator policy for control-plane sizing and replacement.

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ClusterClassSpec configures the control plane of clusters selecting this class.
// +kubebuilder:validation:XValidation:rule="self.controlPlane.replicas > 1 || self.rollout.strategy.rollingUpdate.maxSurge == 1",message="single-replica control planes require maxSurge 1"
type ClusterClassSpec struct {
	// ControlPlane configures control-plane capacity and machine settings.
	ControlPlane ClusterClassControlPlane `json:"controlPlane"`
	// Rollout controls replacement of control-plane nodes.
	Rollout ClusterClassRollout `json:"rollout"`
}

// ClusterClassControlPlane defines control-plane sizing and machine settings.
type ClusterClassControlPlane struct {
	// Replicas is the desired number of control-plane nodes.
	// +required
	// +kubebuilder:validation:Minimum=1
	Replicas *int32 `json:"replicas"`
	// InstanceTypePreferences lists provider-native identifiers in preference order.
	// NICo uses instance-type UUIDs. Providers who use instance-type strings may also use this field.
	// The deployment selects the provider and site used to interpret these values.
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MinLength=1
	InstanceTypePreferences []string `json:"instanceTypePreferences"`
	// SystemReservedMemory is the memory reserved for system daemons on each node.
	// Zero requests no reservation.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="isQuantity(self) && quantity(self).compareTo(quantity('0')) >= 0",message="systemReservedMemory must be a nonnegative Kubernetes quantity"
	SystemReservedMemory string `json:"systemReservedMemory"`
}

// ClusterClassRollout configures automatic replacement of control-plane nodes.
// Consumers must keep downstream automatic replacement enabled.
type ClusterClassRollout struct {
	// Strategy controls replacement order and capacity.
	Strategy ClusterClassRolloutStrategy `json:"strategy"`
}

// ClusterClassRolloutStrategy selects how control-plane nodes are replaced.
type ClusterClassRolloutStrategy struct {
	// Type selects the replacement strategy. Only RollingUpdate is supported.
	// +kubebuilder:validation:Enum=RollingUpdate
	Type string `json:"type"`
	// RollingUpdate bounds additional capacity during replacement.
	RollingUpdate ClusterClassRollingUpdate `json:"rollingUpdate"`
}

// ClusterClassRollingUpdate bounds capacity while replacing nodes incrementally.
type ClusterClassRollingUpdate struct {
	// MaxSurge is the number of extra nodes allowed during replacement.
	// Zero removes first. One waits for healthy replacement capacity before removal.
	// +required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	MaxSurge int32 `json:"maxSurge"`
}

// ClusterClass is operator-published control-plane policy selected by NVKECluster.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,path=clusterclasses,singular=clusterclass
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=".spec.controlPlane.replicas"
type ClusterClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ClusterClassSpec `json:"spec"`
}

// ClusterClassList is a list of published control-plane classes.
// +kubebuilder:object:root=true
type ClusterClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterClass `json:"items"`
}

// init registers class resources so Kubernetes clients can encode and decode them.
func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClusterClass{}, &ClusterClassList{})
		return nil
	})
}
