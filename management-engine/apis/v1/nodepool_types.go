// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// NodePool configures a named group of machines that join the cluster as worker nodes.
type NodePool struct {
	// Name uniquely identifies this pool within the cluster, including across updates.
	Name ResourceName `json:"name"`
	// InstanceType selects the machine type used by the pool and cannot change after creation.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="instanceType is immutable"
	InstanceType ResourceName `json:"instanceType"`
	// Replicas is the required number of nodes requested for this pool. Zero requests no nodes.
	// +required
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas"`
	// VPCSubnetID identifies an existing subnet within spec.network.vpcID for this pool's nodes.
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:MaxLength=36
	VPCSubnetID string `json:"vpcSubnetID"`
	// NodeProfile selects the machine image and startup configuration as <profile>:<tag>.
	NodeProfile NodeProfileReference `json:"nodeProfile"`
	// UserParams configures labels and taints for the pool's nodes.
	// +optional
	UserParams *UserParams `json:"userParams,omitempty"`
	// Rollout limits selection of outdated nodes for replacement after node profile changes.
	// Omission stops new selections after reconciliation. Already-selected nodes may still be replaced.
	// Scaling and fault remediation are unaffected.
	// +optional
	Rollout *Rollout `json:"rollout,omitempty"`
}

// Rollout configures the selection budget for downstream worker replacement.
type Rollout struct {
	Strategy RolloutStrategy `json:"strategy"`
}

// RolloutStrategy selects the recycle-selection policy.
type RolloutStrategy struct {
	// +kubebuilder:validation:Enum=RollingUpdate
	Type       string     `json:"type"`
	Disruption Disruption `json:"disruption"`
}

// Disruption governs new recycle selections, not a guarantee of workload availability.
type Disruption struct {
	// Budgets apply the smallest continuous limit. Percentages use desired replicas and round up.
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	Budgets []DisruptionBudget `json:"budgets"`
}

// DisruptionBudget continuously limits new recycle selections.
type DisruptionBudget struct {
	// Nodes is a nonnegative count or a percentage from 0% to 100% of desired replicas, rounded up.
	// Zero stops only new recycle selections after the policy is applied. Existing work may proceed.
	// +kubebuilder:validation:MaxLength=10
	// +kubebuilder:validation:Pattern=`^((0|[1-9][0-9]*)|([0-9]|[1-9][0-9]|100)%)$`
	// +kubebuilder:validation:XValidation:rule="self.endsWith('%') || int(self) <= 2147483647",message="node count must not exceed 2147483647"
	Nodes string `json:"nodes"`
	// Scheduled windows are deferred until selection and replacement can be coordinated.
	// Schedule string `json:"schedule,omitempty"`
	// Duration string `json:"duration,omitempty"`
}

// NodeProfileReference identifies a node profile and version as <profile>:<tag>.
// +kubebuilder:validation:MaxLength=382
// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*:[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`
// +kubebuilder:validation:XValidation:rule="self.split(':')[0].size() <= 253",message="profile name must be at most 253 characters"
type NodeProfileReference string

// UserParams configures labels and taints for nodes in a pool.
type UserParams struct {
	// Labels specifies node labels. The prefixes nvke.nvidia.com, nke.nvidia.com, kubernetes.io,
	// k8s.io and their subdomains are reserved.
	// +optional
	// +kubebuilder:validation:MaxProperties=128
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.size() <= 317 && k.matches('^([a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$') && (k.split('/').size() == 1 ? k.size() <= 63 : k.split('/')[0].size() <= 253 && k.split('/')[1].size() <= 63))",message="label keys must be Kubernetes qualified names"
	// +kubebuilder:validation:XValidation:rule="self.all(k, !k.matches('(^|[.])(nvke[.]nvidia[.]com|nke[.]nvidia[.]com|kubernetes[.]io|k8s[.]io)/'))",message="platform label prefixes are reserved"
	Labels map[string]LabelValue `json:"labels,omitempty"`
	// Taints specifies node taints that control which pods can be scheduled on these nodes.
	// +optional
	// +listType=map
	// +listMapKey=key
	// +listMapKey=effect
	// +kubebuilder:validation:MaxItems=64
	Taints []NodeTaint `json:"taints,omitempty"`
}

// NodePoolStatus reports the observed state of a cluster's node pool.
type NodePoolStatus struct {
	// Name identifies the corresponding pool in spec.nodePools.
	Name ResourceName `json:"name"`
	// Conditions report pool readiness and progress, with details in reason and message.
	// Each condition's observedGeneration refers to the enclosing NVKECluster's generation.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []NodePoolCondition `json:"conditions,omitempty"`
	// Replicas is the observed number of nodes in the pool. Omitted means unknown.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas,omitempty"`
	// ReadyReplicas is the number of nodes that are ready. Omitted means unknown.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ReadyReplicas *int32 `json:"readyReplicas,omitempty"`
	// UpdatedReplicas is the number of nodes matching the requested pool configuration.
	// Omitted means unknown.
	// +optional
	// +kubebuilder:validation:Minimum=0
	UpdatedReplicas *int32 `json:"updatedReplicas,omitempty"`
	// ID is the NVKE controller's node pool UUID.
	// +optional
	// +kubebuilder:validation:Format=uuid
	ID string `json:"id,omitempty"`
	// CurrentRelease identifies the worker release currently configured in the NVKE controller.
	// +optional
	CurrentRelease *ReleaseReference `json:"currentRelease,omitempty"`
}

// NodePoolCondition reports pool state without a transition timestamp.
type NodePoolCondition struct {
	// +kubebuilder:validation:MaxLength=316
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])$`
	Type string `json:"type"`
	// +kubebuilder:validation:Enum=True;False;Unknown
	Status metav1.ConditionStatus `json:"status"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`
	Reason string `json:"reason"`
	// +kubebuilder:validation:MaxLength=32768
	Message string `json:"message"`
}

// GetConditions returns the pool's conditions for CAPI condition aggregation.
func (p NodePoolStatus) GetConditions() []metav1.Condition {
	conditions := make([]metav1.Condition, len(p.Conditions))
	for i, condition := range p.Conditions {
		conditions[i] = metav1.Condition{
			Type:               condition.Type,
			Status:             condition.Status,
			ObservedGeneration: condition.ObservedGeneration,
			Reason:             condition.Reason,
			Message:            condition.Message,
		}
	}
	return conditions
}

// SetConditions updates the pool's conditions for CAPI condition helpers.
func (p *NodePoolStatus) SetConditions(conditions []metav1.Condition) {
	p.Conditions = make([]NodePoolCondition, len(conditions))
	for i, condition := range conditions {
		p.Conditions[i] = NodePoolCondition{
			Type:               condition.Type,
			Status:             condition.Status,
			ObservedGeneration: condition.ObservedGeneration,
			Reason:             condition.Reason,
			Message:            condition.Message,
		}
	}
}

// GetName identifies the pool in CAPI aggregate condition messages.
func (p NodePoolStatus) GetName() string {
	return string(p.Name)
}

// GetLabels returns nil because embedded node pool status has no resource labels.
func (p NodePoolStatus) GetLabels() map[string]string {
	return nil
}

// LabelValue follows Kubernetes label value syntax.
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
type LabelValue string

// NodeTaint uses Kubernetes label and scheduling conventions.
type NodeTaint struct {
	Key QualifiedName `json:"key"`
	// +optional
	Value LabelValue `json:"value,omitempty"`
	// +kubebuilder:validation:Enum=NoSchedule;PreferNoSchedule;NoExecute
	Effect string `json:"effect"`
}
