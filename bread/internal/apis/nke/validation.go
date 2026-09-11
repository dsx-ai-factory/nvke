// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Readiness validation (NKE-DS9-012, UC-006; docs/PROPOSAL-API-FIELDS.md, NKECluster table) is
// reported to tenants as three conditions on NKECluster (now), NodePool and NKENode (shape):
// Validated is the aggregate and the only one that gates Ready; NetworkValidated covers the
// east-west network checks; AcceleratorValidated covers GPU, NVLink and InfiniBand. Their reason
// is a ValidationState. The condition type names are the frozen surface: operator-defined
// checks roll into Validated and are enumerated by the later ValidationRun kind, never by new
// condition types.
const (
	ConditionValidated            = "Validated"
	ConditionNetworkValidated     = "NetworkValidated"
	ConditionAcceleratorValidated = "AcceleratorValidated"
)

// ValidationState is the state of a readiness validation run or of one of its checks; it is
// also the reason of the three *Validated conditions.
//
// +kubebuilder:validation:Enum=Pending;Running;Passed;Failed
type ValidationState string

// The validation states.
const (
	ValidationPending ValidationState = "Pending"
	ValidationRunning ValidationState = "Running"
	ValidationPassed  ValidationState = "Passed"
	ValidationFailed  ValidationState = "Failed"
)

// ObservedValidation is the machine-provider adapter's readiness report on a cluster, pool or
// node: the aggregate state and the per-check results. It is controller-only hidden state: the
// reconciler folds it into the three *Validated conditions, and the later ValidationRun kind
// surfaces the per-check log to tenants.
type ObservedValidation struct {
	// State is the aggregate: Pending until every check has run, Running while any is, Passed
	// when all passed, Failed when any failed.
	// +kubebuilder:validation:Optional
	State ValidationState `json:"state,omitempty"`

	// Checks are the individual readiness checks the adapter ran, at most one per name.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	Checks []ValidationCheck `json:"checks,omitempty"`
}

// ValidationCheck is the result of one readiness check.
type ValidationCheck struct {
	// Name identifies the check, e.g. basic, east-west-network, gpu, nvlink, infiniband.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// State is the check's state.
	// +kubebuilder:validation:Required
	State ValidationState `json:"state"`

	// Message is the adapter's detail, e.g. why the check failed.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=32768
	Message string `json:"message,omitempty"`

	// CompletedAt is when the check reached Passed or Failed.
	// +kubebuilder:validation:Optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}
