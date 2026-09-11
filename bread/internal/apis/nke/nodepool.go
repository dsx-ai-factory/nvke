// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// NodePool is a group of identically configured worker nodes in an NKECluster.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=nodepools,shortName=np,scope=Namespaced
// +kubebuilder:selectablefield:JSONPath=`.spec.clusterName`
// +kubebuilder:printcolumn:name="CLUSTER",type=string,JSONPath=`.spec.clusterName`
// +kubebuilder:printcolumn:name="INSTANCE-TYPE",type=string,JSONPath=`.spec.instanceType`
// +kubebuilder:printcolumn:name="OS-PROFILE",type=string,JSONPath=`.status.osProfile.name`
// +kubebuilder:printcolumn:name="DESIRED",type=integer,JSONPath=`.status.desiredReplicas`
// +kubebuilder:printcolumn:name="CURRENT",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="UP-TO-DATE",type=integer,JSONPath=`.status.upToDateReplicas`
// +kubebuilder:printcolumn:name="VALIDATED",type=string,JSONPath=`.status.conditions[?(@.type=="Validated")].status`
// +kubebuilder:printcolumn:name="READY",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
type NodePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the tenant's intent.
	// +kubebuilder:validation:Required
	// +nke:external
	Spec NodePoolSpec `json:"spec"`

	// Status is what the controller last observed.
	// +kubebuilder:validation:Optional
	// +nke:external
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

// NodePoolSpec holds the cluster, the instance type, the OS profile, the node count, an optional
// version pin, the node template, the rollout budget and the repair-acceptance policy.
//
// +kubebuilder:validation:XValidation:rule="has(self.replicas) != has(self.autoscaling)",message="set exactly one of replicas or autoscaling"
type NodePoolSpec struct {
	// ClusterName is the metadata.name of the NKECluster the pool belongs to, resolved in the
	// same namespace as the pool; immutable. The field keeps the name clusterName: it names an
	// NKECluster, and "cluster" is what every node-pool API calls the thing a pool belongs to.
	// It is a DNS-1123 label of at most 55 characters, the same rule the NKECluster's own name
	// is held to (registry.HostnameLabelName), since it becomes a hostname.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=55
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterName is immutable"
	// +nke:external
	ClusterName string `json:"clusterName"`

	// InstanceType selects the node hardware class by its concept name, e.g. gb200-nvl72
	// (docs/PROPOSAL-API-FIELDS.md, "Naming"); immutable. It is format-validated only (a
	// DNS-1123 label) until the InstanceType catalog kind lands, when it becomes a reference to
	// that kind without renaming.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="instanceType is immutable"
	// +nke:external
	InstanceType string `json:"instanceType"`

	// OSProfile selects the node OS by family and build (NKE-DS9-005, UC-017): name is the OS
	// family, version an immutable build; version unset means the family's latest supported
	// build, so the pool takes CVE builds as they are published. Changing either is the OS update,
	// rolled under spec.rollout. Both are format-validated strings, resolved against the OSProfile
	// kind. Written by the tenant; resolved by the OS-profile controller.
	// +kubebuilder:validation:Optional
	// +nke:external
	OSProfile *OSProfile `json:"osProfile,omitempty"`

	// Replicas is the fixed node count; exactly one of replicas and autoscaling is set.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=0
	// +nke:external
	Replicas *int32 `json:"replicas,omitempty"`

	// Autoscaling bounds the node count the autoscaler chooses; exactly one of replicas and autoscaling is set.
	// +kubebuilder:validation:Optional
	// +nke:external
	Autoscaling *Autoscaling `json:"autoscaling,omitempty"`

	// KubernetesVersion pins the kubelet version (v1.N.P, at most two minors behind the cluster);
	// unset follows the cluster's control plane. Set, the workers hold while the control plane
	// moves (UC-016); removing the pin is one rolling update to the cluster's version.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="self.matches('^v1[.][0-9]+[.][0-9]+$')",message="must be of the form v1.N.P, for example v1.34.1"
	// +nke:external
	KubernetesVersion *string `json:"kubernetesVersion,omitempty"`

	// Template describes every node in the pool; a template change is the rollout trigger.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default={}
	// +nke:external
	Template *NodeTemplate `json:"template,omitempty"`

	// Rollout is the pool's one disruption budget: it paces how a template, version or OS change
	// reaches the nodes, and bounds how many retire and repair replacements are in flight at
	// once.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default={}
	// +nke:external
	Rollout *Rollout `json:"rollout,omitempty"`

	// RepairAcceptance says whether operator repair requests on this pool's nodes need the
	// tenant's acceptance (UC-010). Manual (default): each request waits for acceptRepair on the
	// NKENode or for its acceptBy deadline. Automatic: requests are accepted at once and the node
	// records acceptedBy: Policy. Not "autoRepair": NKE does not detect and replace nodes on its
	// own; the operator asks, and this says whether to ask the tenant.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=Manual
	// +nke:external
	RepairAcceptance RepairAcceptance `json:"repairAcceptance,omitempty"`
}

// OSProfile selects a node OS by family and build, a catalog selection by concept name
// (docs/PROPOSAL-API-FIELDS.md, "Naming"); it is also the pair the pool's nodes are actually at.
type OSProfile struct {
	// Name is the OS family, e.g. gb200-ubuntu-24.04; the OSProfile kind's metadata.name when it
	// lands. Unset means the instance type's default family.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	// +nke:external
	Name string `json:"name,omitempty"`

	// Version is the immutable build of the family, e.g. 2026.09. Unset means the family's
	// latest supported build; set is a pin.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=64
	// +nke:external
	Version string `json:"version,omitempty"`
}

// RepairAcceptance is a pool's policy for operator repair requests on its nodes.
//
// +kubebuilder:validation:Enum=Manual;Automatic
type RepairAcceptance string

// The repair-acceptance policies.
const (
	// RepairAcceptanceManual: each request needs the tenant's acceptRepair, or its deadline.
	RepairAcceptanceManual RepairAcceptance = "Manual"
	// RepairAcceptanceAutomatic: requests are accepted at once, acceptedBy: Policy.
	RepairAcceptanceAutomatic RepairAcceptance = "Automatic"
)

// Autoscaling holds the inclusive minReplicas and maxReplicas bounds.
//
// +kubebuilder:validation:XValidation:rule="self.minReplicas <= self.maxReplicas",message="minReplicas must not exceed maxReplicas"
type Autoscaling struct {
	// MinReplicas is the lowest node count.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=0
	// +nke:external
	MinReplicas int32 `json:"minReplicas"`

	// MaxReplicas is the highest node count.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +nke:external
	MaxReplicas int32 `json:"maxReplicas"`
}

// NodeTemplate follows PodTemplate naming: metadata for the node object, spec for its configuration.
type NodeTemplate struct {
	// Metadata is applied to every node object.
	// +kubebuilder:validation:Optional
	// +nke:external
	Metadata *NodeTemplateMeta `json:"metadata,omitempty"`

	// Spec configures every node.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default={}
	// +nke:external
	Spec *NodeTemplateSpec `json:"spec,omitempty"`
}

// NodeTemplateMeta holds the node labels.
type NodeTemplateMeta struct {
	// Labels are applied to every node in the pool. Keys under nke.nvidia.com, kubernetes.io and
	// k8s.io, and under any of their subdomains (node-restriction.kubernetes.io, for example), are
	// reserved for the platform and refused.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxProperties=64
	// +kubebuilder:validation:XValidation:rule="self.all(k, !k.matches('(^|[.])(nke[.]nvidia[.]com|kubernetes[.]io|k8s[.]io)/'))",message="keys under nke.nvidia.com, kubernetes.io, k8s.io and their subdomains are reserved for the platform"
	// +nke:external
	Labels map[string]string `json:"labels,omitempty"`
}

// NodeTemplateSpec holds the taints and the kubelet configuration of a node.
type NodeTemplateSpec struct {
	// Taints are applied to every node in the pool. Keys under nke.nvidia.com and its subdomains
	// are reserved for platform health taints and refused.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:XValidation:rule="self.all(t, !t.key.matches('(^|[.])nke[.]nvidia[.]com/'))",message="prefix reserved for platform health taints"
	// +listType=atomic
	// +nke:external
	Taints []Taint `json:"taints,omitempty"`

	// Kubelet is the kubelet configuration of every node; the block where systemReserved and
	// the like go later.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default={}
	// +nke:external
	Kubelet *KubeletConfig `json:"kubelet,omitempty"`
}

// KubeletConfig holds the kubelet settings a tenant may choose.
type KubeletConfig struct {
	// MaxPods caps the pods scheduled on each node (default 110); create-only.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=250
	// +kubebuilder:default=110
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="maxPods is immutable"
	// +nke:external
	MaxPods *int32 `json:"maxPods,omitempty"`
}

// Taint is one node taint: key, optional value and effect.
type Taint struct {
	// Key is the taint key.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +nke:external
	Key string `json:"key"`

	// Value is the taint value.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=63
	// +nke:external
	Value string `json:"value,omitempty"`

	// Effect is what the taint does to pods that do not tolerate it: NoSchedule, PreferNoSchedule or NoExecute.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=NoSchedule;PreferNoSchedule;NoExecute
	// +nke:external
	Effect corev1.TaintEffect `json:"effect"`
}

// Rollout groups the pacing knobs of a node rollout; today that is the strategy.
type Rollout struct {
	// Strategy selects and tunes the rollout algorithm.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default={}
	// +nke:external
	Strategy *RolloutStrategy `json:"strategy,omitempty"`
}

// RolloutStrategyType names a rollout algorithm; the only value is RollingUpdate.
type RolloutStrategyType string

// RollingUpdateStrategyType replaces nodes gradually within maxSurge and maxUnavailable.
const RollingUpdateStrategyType RolloutStrategyType = "RollingUpdate"

// RolloutStrategy holds the algorithm name and the parameters of the one selected.
type RolloutStrategy struct {
	// Type is the rollout algorithm; the only value is RollingUpdate.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Enum=RollingUpdate
	// +kubebuilder:default=RollingUpdate
	// +nke:external
	Type RolloutStrategyType `json:"type,omitempty"`

	// RollingUpdate tunes the RollingUpdate algorithm.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default={}
	// +nke:external
	RollingUpdate *RollingUpdate `json:"rollingUpdate,omitempty"`
}

// RollingUpdate holds maxSurge and maxUnavailable, each a node count or a percentage. The
// defaults (maxSurge 1, maxUnavailable 0; CAPI, GKE and Gardener) scale up a new node before
// an old one goes: on finite bare-metal capacity a surge waits for a machine rather than
// shrinking a GPU pool mid-job.
type RollingUpdate struct {
	// MaxSurge is how many nodes, or what percentage, may exist above the desired count.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=1
	// +nke:external
	MaxSurge *intstr.IntOrString `json:"maxSurge,omitempty"`

	// MaxUnavailable is how many nodes, or what percentage, may be unavailable at once, for any
	// reason: a rollout, a retirement or a repair.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=0
	// +nke:external
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
}

// NodePoolStatus carries the node counts, the kubelet version and conditions.
type NodePoolStatus struct {
	// ObservedGeneration is the metadata.generation this status reflects.
	// +kubebuilder:validation:Optional
	// +nke:external
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// DesiredReplicas is the node count the pool is converging to: replicas, or the autoscaler's choice.
	// +kubebuilder:validation:Optional
	// +nke:external
	DesiredReplicas int32 `json:"desiredReplicas,omitempty"`

	// Replicas is the number of nodes that exist.
	// +kubebuilder:validation:Optional
	// +nke:external
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas is the number of nodes that are Ready.
	// +kubebuilder:validation:Optional
	// +nke:external
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// UpToDateReplicas is the number of nodes at the current template and version.
	// +kubebuilder:validation:Optional
	// +nke:external
	UpToDateReplicas int32 `json:"upToDateReplicas,omitempty"`

	// KubernetesVersion is the kubelet version the nodes run: the pinned version, else the cluster's.
	// +kubebuilder:validation:Optional
	// +nke:external
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`

	// OSProfile is the OS family and build the nodes are actually at; it lags spec.osProfile
	// during a rollout (UC-017). Written by the reconciler from the machine-provider adapter's
	// report.
	// +kubebuilder:validation:Optional
	// +nke:external
	OSProfile *OSProfile `json:"osProfile,omitempty"`

	// Conditions are Ready (reasons AllNodesReady, Scaling, ClusterNotFound, ClusterReplaced),
	// AtDesiredSize (reasons AtDesiredSize, Scaling), RollingOut (reasons Idle, Rolling,
	// DrainBlocked with the blocking PodDisruptionBudget in the message, WaitingForCapacity),
	// VersionSkewWithinTolerance (reasons FollowsCluster, WithinTolerance, SkewExceeded,
	// ClusterVersionUnknown),
	// RepairsPending (True while any node of the pool has an open repairRequest; the message counts
	// them and names the earliest deadline) and the readiness validation conditions Validated,
	// NetworkValidated and AcceleratorValidated (reasons Pending, Running, Passed, Failed), which
	// summarize the readiness checks the machine-provider adapter reports for the pool.
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +nke:external
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Internal is controller-only state the tenant never sees.
	// +kubebuilder:validation:Optional
	Internal NodePoolInternalState `json:"internal,omitempty"`
}

// NodePoolInternalState is the controller-only state of a node pool (docs/PROPOSAL-API-FIELDS.md,
// "Hidden fields"). The machine-provider adapter owns the observed* fields and writes them by
// merge patch; the reconciler owns the rest.
type NodePoolInternalState struct {
	// BoundClusterUID is the uid of the NKECluster the pool was first resolved against; a
	// different uid under the same name is a replaced cluster, which the pool never adopts.
	// +kubebuilder:validation:Optional
	BoundClusterUID types.UID `json:"boundClusterUID,omitempty"`

	// ObservedRevision is the template-and-version revision the machine provider reports the
	// nodes at.
	// +kubebuilder:validation:Optional
	ObservedRevision string `json:"observedRevision,omitempty"`

	// ObservedKubernetesVersion is the kubelet version the machine provider reports.
	// +kubebuilder:validation:Optional
	ObservedKubernetesVersion string `json:"observedKubernetesVersion,omitempty"`

	// ObservedValidation is the machine provider's readiness report on the pool; the reconciler
	// folds it into the Validated, NetworkValidated and AcceleratorValidated conditions.
	// +kubebuilder:validation:Optional
	ObservedValidation *ObservedValidation `json:"observedValidation,omitempty"`

	// ProviderRefs names the substrate objects behind the pool, keyed by role.
	// +kubebuilder:validation:Optional
	ProviderRefs map[string]string `json:"providerRefs,omitempty"`
}
