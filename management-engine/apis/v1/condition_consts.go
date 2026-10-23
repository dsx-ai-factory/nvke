// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

const ControlPlaneReadyCondition = "ControlPlaneReady"

const (
	ClusterReconciledCondition = "ClusterReconciled"
	ReconcileSucceededReason   = "ReconcileSucceeded"
	ReconcileFailedReason      = "ReconcileFailed"
)

const (
	NVKEControllerProvisionedCondition           = "NVKEControllerProvisioned"
	NVKEControllerProvisioningCondition          = "NVKEControllerProvisioning"
	NVKEControllerDeletingCondition              = "NVKEControllerDeleting"
	NVKEControllerControlPlaneUpgradingCondition = "NVKEControllerControlPlaneUpgrading"
	NVKEControllerCPNodesRecyclingCondition      = "NVKEControllerCPNodesRecycling"
	NVKEControllerUpdateFailedCondition          = "NVKEControllerUpdateFailed"
	ReportedReason                               = "Reported"
	NotYetReportedReason                         = "NotYetReported"
)

const (
	SyncedCondition   = "Synced"
	SyncedReason      = "Synced"
	NotSyncedReason   = "NotSynced"
	SyncUnknownReason = "SyncUnknown"
)
