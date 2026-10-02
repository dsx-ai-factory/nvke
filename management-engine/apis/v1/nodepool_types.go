// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// NodePoolSpec describes workers managed through an upstream MachineDeployment.
type NodePoolSpec struct {
	// ClusterName is resolved only within this NodePool's namespace.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterName is immutable"
	ClusterName ResourceName `json:"clusterName"`
	// Replicas is required. Zero explicitly requests no workers; omission is
	// invalid. A pointer preserves this distinction in Go and future projections.
	// +required
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas"`
	// InstanceType names an operator offering, not a provider-specific ID.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="instanceType is immutable"
	InstanceType ResourceName     `json:"instanceType"`
	Release      ReleaseReference `json:"release"`
	// Template changes update labels and taints in place.
	// +optional
	Template *NodeTemplate `json:"template,omitempty"`
	// +optional
	// +kubebuilder:default={strategy:{type:RollingUpdate,rollingUpdate:{maxSurge:1}}}
	Rollout *NodePoolRollout `json:"rollout,omitempty"`
}

// NodeTemplate contains only the tenant-controlled labels and taints of workers.
type NodeTemplate struct {
	// +optional
	Metadata *NodeTemplateMetadata `json:"metadata,omitempty"`
	// +optional
	Spec *NodeTemplateSpec `json:"spec,omitempty"`
}

// NodeTemplateMetadata is a limited template, not a full Kubernetes ObjectMeta.
type NodeTemplateMetadata struct {
	// Labels are propagated to existing workers without recycling Machines.
	// Platform domains and their subdomains are reserved.
	// +optional
	// +kubebuilder:validation:MaxProperties=128
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.size() <= 317 && k.matches('^([a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$') && (k.split('/').size() == 1 ? k.size() <= 63 : k.split('/')[0].size() <= 253 && k.split('/')[1].size() <= 63))",message="label keys must be Kubernetes qualified names"
	// +kubebuilder:validation:XValidation:rule="self.all(k, !k.matches('(^|[.])(nke[.]nvidia[.]com|kubernetes[.]io|k8s[.]io)/'))",message="platform label prefixes are reserved"
	Labels map[string]LabelValue `json:"labels,omitempty"`
}

// LabelValue follows Kubernetes label value syntax.
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
type LabelValue string

// NodeTemplateSpec holds tenant-controlled scheduling properties.
type NodeTemplateSpec struct {
	// +optional
	// +listType=map
	// +listMapKey=key
	// +listMapKey=effect
	// +kubebuilder:validation:MaxItems=64
	Taints []NodeTaint `json:"taints,omitempty"`
}

// NodeTaint uses Kubernetes' conventions
type NodeTaint struct {
	Key QualifiedName `json:"key"`
	// +optional
	Value LabelValue `json:"value,omitempty"`
	// +kubebuilder:validation:Enum=NoSchedule;PreferNoSchedule;NoExecute
	Effect string `json:"effect"`
}

// NodePoolRollout configures release replacement through upstream CAPI.
type NodePoolRollout struct {
	Strategy NodePoolRolloutStrategy `json:"strategy"`
}

// NodePoolRolloutStrategy initially supports only rolling replacement.
type NodePoolRolloutStrategy struct {
	// +kubebuilder:validation:Enum=RollingUpdate
	Type          string                `json:"type"`
	RollingUpdate RollingUpdateStrategy `json:"rollingUpdate"`
}

// RollingUpdateStrategy bounds temporary capacity above the desired count.
type RollingUpdateStrategy struct {
	// MaxSurge accepts an int32 count or a percentage with an int32 numerator.
	// This bounds representation, not the operational capacity of a NodePool.
	// +kubebuilder:validation:XIntOrString
	// +kubebuilder:validation:XValidation:rule="type(self) == int ? self >= 0 && self <= 2147483647 : self.matches('^(0|[1-9][0-9]{0,9})%$') && int(self.substring(0, self.size() - 1)) <= 2147483647",message="must be an integer or percentage between 0 and 2147483647"
	MaxSurge intstr.IntOrString `json:"maxSurge"`
}

// NodePoolStatus reports observed Machines and the immutable cluster binding.
type NodePoolStatus struct {
	// ClusterRef is controller-owned. Once resolved, a different UID under the
	// same name must be reported as a stale reference, never adopted.
	// +optional
	ClusterRef *ClusterReference `json:"clusterRef,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Replicas is the observed Machine count; absent means not yet observed.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	ReadyReplicas *int32 `json:"readyReplicas,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	UpdatedReplicas *int32 `json:"updatedReplicas,omitempty"`
	// +optional
	ResolvedRelease *ReleaseReference `json:"resolvedRelease,omitempty"`
}

// NodePool is the internal representation of a workload cluster's worker pool.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,path=nodepools,singular=nodepool
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.clusterRef) || (has(self.status) && has(self.status.clusterRef) && self.status.clusterRef == oldSelf.status.clusterRef)",message="clusterRef cannot be changed or removed once bound"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.clusterRef) || self.status.clusterRef.name == self.spec.clusterName",message="clusterRef must match spec.clusterName"
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.clusterName"
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=".spec.replicas"
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=".status.readyReplicas"
type NodePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NodePoolSpec `json:"spec"`
	// +optional
	Status NodePoolStatus `json:"status,omitempty,omitzero"`
}

// NodePoolList is a list of internal worker pools.
// +kubebuilder:object:root=true
type NodePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodePool `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &NodePool{}, &NodePoolList{})
		return nil
	})
}

func (p *NodePool) GetConditions() []metav1.Condition {
	return p.Status.Conditions
}

func (p *NodePool) SetConditions(conditions []metav1.Condition) {
	p.Status.Conditions = conditions
}
