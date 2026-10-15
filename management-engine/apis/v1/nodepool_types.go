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
	// Rollout configures node replacement when the node profile changes.
	// +optional
	Rollout *Rollout `json:"rollout,omitempty"`
}

// Rollout configures how changes are applied to nodes.
type Rollout struct {
	Strategy RolloutStrategy `json:"strategy"`
}

// RolloutStrategy selects the replacement method and disruption limits.
type RolloutStrategy struct {
	// +kubebuilder:validation:Enum=RollingUpdate
	Type       string     `json:"type"`
	Disruption Disruption `json:"disruption"`
}

// Disruption limits how many nodes may be disrupted during replacement.
type Disruption struct {
	// Budgets apply the smallest active limit. Percentages round up to whole nodes.
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	Budgets []DisruptionBudget `json:"budgets"`
}

// DisruptionBudget limits disruption continuously or during a scheduled window.
// +kubebuilder:validation:XValidation:rule="has(self.schedule) == has(self.duration)",message="schedule and duration must be set together"
type DisruptionBudget struct {
	// Nodes is a nonnegative count or a percentage from 0% to 100%. Zero blocks disruption.
	// +kubebuilder:validation:MaxLength=10
	// +kubebuilder:validation:Pattern=`^((0|[1-9][0-9]*)|([0-9]|[1-9][0-9]|100)%)$`
	// +kubebuilder:validation:XValidation:rule="self.endsWith('%') || int(self) <= 2147483647",message="node count must not exceed 2147483647"
	Nodes string `json:"nodes"`
	// Schedule starts each active window using five-field cron or a standard alias such as @daily.
	// Omit for a continuous budget.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[[:space:]]*(@(annually|yearly|monthly|weekly|daily|midnight|hourly)|([^[:space:]]+[[:space:]]+){4}[^[:space:]]+)[[:space:]]*$`
	Schedule string `json:"schedule,omitempty"`
	// Duration is the active window length, expressed in hours, minutes or seconds, such as 8h or 30m.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^([0-9]+h)?([0-9]+m)?([0-9]+s)?$`
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="duration must be positive"
	Duration string `json:"duration,omitempty"`
}

// NodeProfileReference identifies a node profile and version as <profile>:<tag>.
// +kubebuilder:validation:MaxLength=382
// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*:[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`
// +kubebuilder:validation:XValidation:rule="self.split(':')[0].size() <= 253",message="profile name must be at most 253 characters"
type NodeProfileReference string

// UserParams configures labels and taints for nodes in a pool.
type UserParams struct {
	// Labels specifies node labels. The prefixes nke.nvidia.com, kubernetes.io,
	// k8s.io and their subdomains are reserved.
	// +optional
	// +kubebuilder:validation:MaxProperties=128
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.size() <= 317 && k.matches('^([a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$') && (k.split('/').size() == 1 ? k.size() <= 63 : k.split('/')[0].size() <= 253 && k.split('/')[1].size() <= 63))",message="label keys must be Kubernetes qualified names"
	// +kubebuilder:validation:XValidation:rule="self.all(k, !k.matches('(^|[.])(nke[.]nvidia[.]com|kubernetes[.]io|k8s[.]io)/'))",message="platform label prefixes are reserved"
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
	// ObservedGeneration refers to the enclosing NVKECluster's generation.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions report pool readiness and progress, with details in reason and message.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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
