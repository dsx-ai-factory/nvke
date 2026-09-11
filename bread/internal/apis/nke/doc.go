// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nke is the hub (internal) version of the nke API group: the tenant kinds NKECluster,
// NodePool and NKENode, the operator kinds Release and ReleaseChannel and the controller-owned
// ClusterRelease, each with every field the controller needs.
//
// This package is the single source of truth. Only fields marked `+nke:external` are tenant-facing;
// an unmarked field is controller-only and hidden by default. `cmd/nke-gen` emits into v1/ the
// public types with the marked fields alone, the `<Kind>Internal` types with every field, the
// conversions between the three, and the table convertors. Nothing outside internal/apiserver,
// internal/apis/nke/management and internal/reconcile should depend on this package directly.
//
// +kubebuilder:object:generate=true
// +k8s:deepcopy-gen=package
// +groupName=nke
package nke

// GroupName is the API group this package is the hub version of. It is a dotless group
// (docs/DECISIONS.md §33): nke-apiserver serves it itself and needs no CRD, so the dot rule
// for CRD groups does not apply; the stored shape uses internal.nke.nvidia.com (D2).
const GroupName = "nke"
