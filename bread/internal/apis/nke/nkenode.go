// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// NKENode is one worker machine of a NodePool as its tenant sees it (docs/PROPOSAL-API-FIELDS.md,
// NKENode): the per-machine record break-fix (UC-010) negotiates over. The controller creates
// one per machine, owned by the pool, and deletes it once the machine is released; tenants never
// create or delete one. Tenants write spec (retire, acceptRepair) and read status. Kind NKENode,
// not Node, so `kubectl get nodes` keeps meaning the workload cluster's nodes.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=nkenodes,shortName=nkeno,scope=Namespaced
// +kubebuilder:selectablefield:JSONPath=`.status.clusterName`
// +kubebuilder:selectablefield:JSONPath=`.status.nodePoolName`
// +kubebuilder:printcolumn:name="POOL",type=string,JSONPath=`.status.nodePoolName`
// +kubebuilder:printcolumn:name="HEALTHY",type=string,JSONPath=`.status.conditions[?(@.type=="Healthy")].status`
// +kubebuilder:printcolumn:name="VALIDATED",type=string,JSONPath=`.status.conditions[?(@.type=="Validated")].status`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
type NKENode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the tenant's intent for this one machine.
	// +kubebuilder:validation:Required
	// +nke:external
	Spec NKENodeSpec `json:"spec"`

	// Status is what the controller last observed.
	// +kubebuilder:validation:Optional
	// +nke:external
	Status NKENodeStatus `json:"status,omitempty"`
}

// NKENodeList is a list of NKENodes.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type NKENodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NKENode `json:"items"`
}

// NKENodeSpec holds the two tenant verbs on a machine: return it, or accept the operator's
// request to repair it.
//
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.retire) || (has(self.retire) && self.retire == oldSelf.retire)",message="retire is immutable once set"
type NKENodeSpec struct {
	// Retire is the tenant-initiated return of this machine (UC-010, UC-015); immutable once
	// set. NKE cordons the node, drains it under the applicable disruption budget, detaches it
	// and returns it to the provider (NKE-DS9-001); the pool's desired count is unchanged, so
	// the pool reconciler provisions a replacement and deletes this object once Retiring
	// completes.
	// +kubebuilder:validation:Optional
	// +nke:external
	Retire *RetireRequest `json:"retire,omitempty"`

	// AcceptRepair is the tenant's standing acceptance of repair requests on this machine: true
	// means any status.repairRequest is accepted (acceptedBy: Tenant), the per-node form of the
	// pool's repairAcceptance: Automatic, so a request opened, withdrawn and reopened is accepted
	// again without a new write. It is a persistent setting, never a one-shot verb and never
	// reinterpreted as "only the current request". Ignored while no request is open.
	// +kubebuilder:validation:Optional
	// +nke:external
	AcceptRepair bool `json:"acceptRepair,omitempty"`
}

// RetireRequest is the tenant's reason for returning a machine.
type RetireRequest struct {
	// Reason says why the machine is being returned, for the operator's record.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=1024
	// +nke:external
	Reason string `json:"reason,omitempty"`
}

// NKENodeStatus carries the machine's place (cluster and pool), what it runs, the open repair
// request and conditions.
type NKENodeStatus struct {
	// ObservedGeneration is the metadata.generation this status reflects.
	// +kubebuilder:validation:Optional
	// +nke:external
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ClusterName is the metadata.name of the NKECluster the machine belongs to, denormalised
	// from the owning pool; a selectable field.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=55
	// +nke:external
	ClusterName string `json:"clusterName,omitempty"`

	// NodePoolName is the metadata.name of the owning NodePool; a selectable field.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=55
	// +nke:external
	NodePoolName string `json:"nodePoolName,omitempty"`

	// KubernetesVersion is the kubelet version this machine runs; it lags the pool's during a
	// rollout.
	// +kubebuilder:validation:Optional
	// +nke:external
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`

	// Labels are the labels observed on the workload cluster's node.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxProperties=64
	// +nke:external
	Labels map[string]string `json:"labels,omitempty"`

	// Taints are the taints observed on the workload cluster's node.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	// +nke:external
	Taints []Taint `json:"taints,omitempty"`

	// RepairRequest is the operator's open request to take the machine for repair (UC-010; the
	// GCE upcomingMaintenance shape). Written by the operator on the internal group, read-only
	// to the tenant, answered by spec.acceptRepair. Lifecycle: open, then accepted (acceptedBy,
	// acceptedAt), then Retiring, then deleted; or withdrawn, when the operator clears the whole
	// block, the one defined cancel.
	// +kubebuilder:validation:Optional
	// +nke:external
	RepairRequest *RepairRequest `json:"repairRequest,omitempty"`

	// Conditions are Ready (the node is Ready in the workload cluster), Healthy (the machine
	// provider reports the machine healthy), RepairRequested (True while a request is open, reasons
	// AwaitingTenant or Accepted, a passed acceptBy deadline being an acceptance with acceptedBy
	// Deadline; False with reason None or Withdrawn), Retiring (True from a retire or an accepted
	// repair until the machine is released and this object deleted: reason Pending while the pool
	// holds the release for its rollout budget and the machine still serves, Releasing once the
	// pool clears it, which is the machine provider's one cue to release; False with reason None),
	// and the readiness validation conditions Validated, NetworkValidated and AcceleratorValidated
	// (reasons Pending, Running, Passed, Failed), which summarize the readiness checks the
	// machine-provider adapter reports for the node.
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +nke:external
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Internal is controller-only state the tenant never sees.
	// +kubebuilder:validation:Optional
	Internal NKENodeInternalState `json:"internal,omitempty"`
}

// RepairAcceptor says who accepted a repair request.
//
// +kubebuilder:validation:Enum=Tenant;Policy;Deadline
type RepairAcceptor string

// The ways a repair request is accepted.
const (
	// RepairAcceptedByTenant: the tenant set spec.acceptRepair.
	RepairAcceptedByTenant RepairAcceptor = "Tenant"
	// RepairAcceptedByPolicy: the pool's spec.repairAcceptance: Automatic accepted it at once.
	RepairAcceptedByPolicy RepairAcceptor = "Policy"
	// RepairAcceptedByDeadline: acceptBy passed without an answer.
	RepairAcceptedByDeadline RepairAcceptor = "Deadline"
)

// RepairRequest is the operator's request to take a machine for repair.
type RepairRequest struct {
	// RequestedAt is when the operator asked.
	// +kubebuilder:validation:Required
	// +nke:external
	RequestedAt metav1.Time `json:"requestedAt"`

	// Reason is the operator's reason, e.g. "DIMM errors on host".
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=1024
	// +nke:external
	Reason string `json:"reason,omitempty"`

	// AcceptBy is the operator's deadline: after it the request proceeds without the tenant's
	// acceptance and acceptedBy records Deadline.
	// +kubebuilder:validation:Optional
	// +nke:external
	AcceptBy *metav1.Time `json:"acceptBy,omitempty"`

	// AcceptedBy is Tenant, Policy or Deadline once the request has been accepted; unset while
	// it is open.
	// +kubebuilder:validation:Optional
	// +nke:external
	AcceptedBy RepairAcceptor `json:"acceptedBy,omitempty"`

	// AcceptedAt is when the request was accepted; unset while it is open.
	// +kubebuilder:validation:Optional
	// +nke:external
	AcceptedAt *metav1.Time `json:"acceptedAt,omitempty"`
}

// NKENodeInternalState is the controller-only state of a machine (docs/PROPOSAL-API-FIELDS.md,
// "Hidden fields"). The machine-provider adapter owns the observed* fields and writes them by
// merge patch; the reconciler owns the rest.
type NKENodeInternalState struct {
	// BoundPoolUID is the uid of the NodePool the machine was created for; a different uid under
	// the same name is a replaced pool, which never adopts the machine.
	// +kubebuilder:validation:Optional
	BoundPoolUID types.UID `json:"boundPoolUID,omitempty"`

	// Replaces is the metadata.name of the retired or repaired NKENode this one was provisioned
	// to replace.
	// +kubebuilder:validation:Optional
	Replaces string `json:"replaces,omitempty"`

	// ReplacedBy is the metadata.name of the NKENode provisioned to replace this one; set while
	// this one is Retiring, so pool-level replacement knows the retirement is complete.
	// +kubebuilder:validation:Optional
	ReplacedBy string `json:"replacedBy,omitempty"`

	// ObservedReady is whether the machine provider reports the node Ready in the workload cluster.
	// +kubebuilder:validation:Optional
	ObservedReady bool `json:"observedReady,omitempty"`

	// ObservedHealthy is whether the machine provider reports the machine healthy.
	// +kubebuilder:validation:Optional
	ObservedHealthy bool `json:"observedHealthy,omitempty"`

	// ObservedLabels are the labels the machine provider reports on the workload node; the
	// reconciler projects them to status.labels.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxProperties=64
	ObservedLabels map[string]string `json:"observedLabels,omitempty"`

	// ObservedTaints are the taints the machine provider reports on the workload node; the
	// reconciler projects them to status.taints.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	ObservedTaints []Taint `json:"observedTaints,omitempty"`

	// ObservedValidation is the machine provider's readiness report on the machine; the
	// reconciler folds it into the Validated, NetworkValidated and AcceleratorValidated conditions.
	// +kubebuilder:validation:Optional
	ObservedValidation *ObservedValidation `json:"observedValidation,omitempty"`
}
