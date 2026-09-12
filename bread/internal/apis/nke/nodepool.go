// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// NodePool is a group of identically configured worker nodes in an NKECluster (UC-013, UC-015).
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas
// +kubebuilder:resource:path=nodepools,shortName=np,scope=Namespaced
// +kubebuilder:selectablefield:JSONPath=`.spec.clusterName`
// +kubebuilder:printcolumn:name="CLUSTER",type=string,JSONPath=`.spec.clusterName`
// +kubebuilder:printcolumn:name="INSTANCE-TYPE",type=string,JSONPath=`.spec.instanceType`
// +kubebuilder:printcolumn:name="IMAGE",type=string,JSONPath=`.status.machineImage.name`
// +kubebuilder:printcolumn:name="DESIRED",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="CURRENT",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="UP-TO-DATE",type=integer,JSONPath=`.status.upToDateReplicas`
// +kubebuilder:printcolumn:name="READY",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
type NodePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the tenant's intent.
	// +kubebuilder:validation:Required
	// +nke:tenant=rw
	// +nke:operator=r
	Spec NodePoolSpec `json:"spec"`

	// Status is what the controller last observed.
	// +kubebuilder:validation:Optional
	// +nke:tenant=r
	// +nke:operator=r
	Status NodePoolStatus `json:"status,omitempty"`
}

// NodePoolList is a list of NodePools.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type NodePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodePool `json:"items"`
}

// NodePoolSpec holds the cluster, the instance type, the machine image, the node count, the
// node template and the rollout budget.
type NodePoolSpec struct {
	// ClusterName is the metadata.name of the NKECluster the pool belongs to, resolved in the
	// same namespace as the pool; immutable. On first resolution the controller sets an owner
	// reference to that cluster, uid included, and the pool stays bound to that uid. It is a
	// DNS-1123 label of at most 55 characters, the rule the NKECluster's own name is held to,
	// since it becomes a hostname.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=55
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterName is immutable"
	// +nke:tenant=rw
	// +nke:operator=r
	ClusterName string `json:"clusterName"`

	// InstanceType selects the node hardware class by name, e.g. gb200-nvl72 (UC-002); immutable.
	// It is format-validated only (a DNS-1123 label) until the InstanceType catalog kind lands,
	// when it becomes a reference to that kind without renaming.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="instanceType is immutable"
	// +nke:tenant=rw
	// +nke:operator=r
	InstanceType string `json:"instanceType"`

	// MachineImage selects the node OS image the operator registered (UC-002, NKE-DS9-005).
	// Unset means the instance type's default image.
	// +kubebuilder:validation:Optional
	// +nke:tenant=rw
	// +nke:operator=r
	MachineImage *MachineImage `json:"machineImage,omitempty"`

	// Replicas is the node count. Scaling it is the one operation the /scale subresource exposes.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=0
	// +nke:tenant=rw
	// +nke:operator=r
	Replicas int32 `json:"replicas"`

	// PLACEHOLDER (UC-016, P1): a per-pool Kubernetes version within the supported skew of the
	// cluster's Release, so workers can hold while the control plane moves.
	//
	//   KubernetesVersion *string `json:"kubernetesVersion,omitempty"`
	//
	// NKE today takes a per-pool kubernetesVersion (X.Y.Z, update-only) as the worker-upgrade
	// trigger. Here the worker version arrives with the Release, so the field only returns if the
	// skew story needs it.

	// Template describes every node in the pool; a template change is a rollout.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default={}
	// +nke:tenant=rw
	// +nke:operator=r
	Template *NodeTemplate `json:"template,omitempty"`

	// Rollout is the pool's one disruption budget: it paces how an image or template change
	// reaches the nodes and bounds how many retire and repair replacements are in flight at once
	// (UC-016, UC-017).
	// +kubebuilder:validation:Optional
	// +kubebuilder:default={}
	// +nke:tenant=rw
	// +nke:operator=r
	Rollout *Rollout `json:"rollout,omitempty"`

	// PLACEHOLDER (UC-010): whether operator repair requests on this pool's nodes need the
	// tenant's acceptance. The SRD's "tenant may configure auto-repair".
	//
	//   RepairAcceptance RepairAcceptance `json:"repairAcceptance,omitempty"`   // Manual (default) | Automatic
	//
	// Not implemented in NKE, where repair is the tenant deleting the node with a health issue.
}

// MachineImage selects a node OS image by name. It is also the image the pool's nodes are
// actually at, in status.
type MachineImage struct {
	// Name is the image the operator registered, e.g. gb200-ubuntu-24.04; the MachineImage
	// catalog kind's metadata.name when it lands. Today NKE calls this the OS profile.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	// +nke:tenant=rw
	// +nke:operator=r
	Name string `json:"name,omitempty"`

	// PLACEHOLDER (UC-017, P1): an immutable build of the image, so a pool can take a CVE build
	// without changing image, rolled under spec.rollout.
	//
	//   Version string `json:"version,omitempty"`
	//
	// NKE registers an image as one name today, with no build dimension.
}

// NodeTemplate follows PodTemplate naming: metadata for the node object. A spec block for node
// configuration is not needed yet: kubelet tuning and taints are not in the SRD or in NKE.
type NodeTemplate struct {
	// Metadata is applied to every node object.
	// +kubebuilder:validation:Optional
	// +nke:tenant=rw
	// +nke:operator=r
	Metadata *NodeTemplateMeta `json:"metadata,omitempty"`
}

// NodeTemplateMeta holds the node labels.
type NodeTemplateMeta struct {
	// Labels are applied to every node in the pool (NKE-DS9-020 covers the platform's own
	// topology labels, which arrive on the node without being asked for). Keys under
	// nke.nvidia.com, kubernetes.io and k8s.io, and under any of their subdomains, are reserved
	// for the platform and refused.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxProperties=64
	// +kubebuilder:validation:XValidation:rule="self.all(k, !k.matches('(^|[.])(nke[.]nvidia[.]com|kubernetes[.]io|k8s[.]io)/'))",message="keys under nke.nvidia.com, kubernetes.io, k8s.io and their subdomains are reserved for the platform"
	// +nke:tenant=rw
	// +nke:operator=r
	Labels map[string]string `json:"labels,omitempty"`
}

// Rollout groups the pacing knobs of a node rollout; today that is the strategy.
type Rollout struct {
	// Strategy selects and tunes the rollout algorithm.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default={}
	// +nke:tenant=rw
	// +nke:operator=r
	Strategy *RolloutStrategy `json:"strategy,omitempty"`
}

// RolloutStrategyType names a rollout algorithm; the only value is RollingUpdate.
type RolloutStrategyType string

// RollingUpdateStrategyType replaces nodes gradually within the budget.
const RollingUpdateStrategyType RolloutStrategyType = "RollingUpdate"

// RolloutStrategy holds the algorithm name and the parameters of the one selected.
type RolloutStrategy struct {
	// Type is the rollout algorithm; the only value is RollingUpdate.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Enum=RollingUpdate
	// +kubebuilder:default=RollingUpdate
	// +nke:tenant=rw
	// +nke:operator=r
	Type RolloutStrategyType `json:"type,omitempty"`

	// RollingUpdate tunes the RollingUpdate algorithm.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default={}
	// +nke:tenant=rw
	// +nke:operator=r
	RollingUpdate *RollingUpdate `json:"rollingUpdate,omitempty"`
}

// RollingUpdate holds the surge budget. The default (maxSurge 1; CAPI, GKE and Gardener) brings
// a new node up before an old one goes: on finite bare-metal capacity a surge waits for a
// machine rather than shrinking a GPU pool mid-job. The SRD's "applicable disruption budget"
// (NKE-DS9-001, UC-016, UC-017) is this block.
type RollingUpdate struct {
	// MaxSurge is how many nodes, or what percentage, may exist above the desired count. Today
	// NKE's maxSurge and recycleBudget.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=1
	// +nke:tenant=rw
	// +nke:operator=r
	MaxSurge *intstr.IntOrString `json:"maxSurge,omitempty"`

	// PLACEHOLDER (UC-016, UC-017): how many nodes may be unavailable at once, for any reason.
	//
	//   MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`   // default 0
	//
	// Not implemented in NKE, which rolls sequentially when maxSurge is 0.
}

// NodePoolStatus carries the node counts, the running version and image, and conditions.
type NodePoolStatus struct {
	// ObservedGeneration is the metadata.generation this status reflects.
	// +kubebuilder:validation:Optional
	// +nke:tenant=r
	// +nke:operator=r
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Replicas is the number of nodes that exist.
	// +kubebuilder:validation:Optional
	// +nke:tenant=r
	// +nke:operator=r
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas is the number of nodes that are Ready.
	// +kubebuilder:validation:Optional
	// +nke:tenant=r
	// +nke:operator=r
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// UpToDateReplicas is the number of nodes at the current template and image.
	// +kubebuilder:validation:Optional
	// +nke:tenant=r
	// +nke:operator=r
	UpToDateReplicas int32 `json:"upToDateReplicas,omitempty"`

	// KubernetesVersion is the kubelet version the nodes run; it lags the cluster's during an
	// upgrade.
	// +kubebuilder:validation:Optional
	// +nke:tenant=r
	// +nke:operator=r
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`

	// MachineImage is the image the nodes are actually at; it lags spec.machineImage during a
	// rollout.
	// +kubebuilder:validation:Optional
	// +nke:tenant=r
	// +nke:operator=r
	MachineImage *MachineImage `json:"machineImage,omitempty"`

	// Conditions are Ready (reasons AllNodesReady, Scaling, ClusterNotFound, ClusterReplaced),
	// AtDesiredSize (reasons AtDesiredSize, Scaling) and RollingOut (reasons Idle, Rolling,
	// DrainBlocked with the blocking PodDisruptionBudget in the message, WaitingForCapacity).
	// ClusterReplaced is raised from the owner reference: the NKECluster now under
	// spec.clusterName has a different uid than the one the pool was bound to at creation.
	//
	// PLACEHOLDER conditions, added with their features: VersionSkewWithinTolerance (UC-016),
	// RepairsPending (UC-010; True while any node of the pool has an open repair request) and the
	// readiness validation conditions Validated, NetworkValidated, AcceleratorValidated
	// (NKE-DS9-012; see validation.go).
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +nke:tenant=r
	// +nke:operator=r
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// There is no controller-only state on a pool. Which cluster the pool is bound to is the
	// owner reference the controller sets on first resolution (uid included, so a recreated
	// cluster under the same name is never adopted); which revision the nodes are at is read
	// from the backend on every reconcile and reported as upToDateReplicas, kubernetesVersion
	// and machineImage above.
}
