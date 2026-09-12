// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

// PLACEHOLDER (NKE-DS9-012, UC-006): readiness validation. A cluster, pool or scalable unit must
// pass basic, east-west network and GPU/NVLink/InfiniBand validation before it is presented as
// usable, and the tenant must be able to see the progress. Not implemented in NKE, whose
// readiness surface is the per-node kubernetesReady and nodeAgentReady conditions.
//
// The shape, to revisit with implementation: three conditions on NKECluster, NodePool and
// NKENode, with a shared reason vocabulary. Validated is the aggregate and the only one that
// gates Ready; NetworkValidated covers the east-west checks; AcceleratorValidated covers GPU,
// NVLink and InfiniBand. Operator-defined checks (the SRD's "extensible") roll into Validated and
// are enumerated by a later ValidationRun kind, never by new condition types.
//
//	const (
//		ConditionValidated            = "Validated"
//		ConditionNetworkValidated     = "NetworkValidated"
//		ConditionAcceleratorValidated = "AcceleratorValidated"
//	)
//
//	// ValidationState is the reason of the three conditions.
//	// +kubebuilder:validation:Enum=Pending;Running;Passed;Failed
//	type ValidationState string
