// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Condition types published by the provider's reconcilers.
//
// When adding a reason that means a control-plane NicoMachine is genuinely
// waiting on instance-type capacity -- one a freed allocation would actually
// unblock -- also add it to capacityWaitReasons in controllers/helpers.go.
// That list is an allowlist, so a reason left out simply does not defer
// workers; nothing needs doing for reasons capacity cannot fix.
const (
	// NicoReadyCondition reports whether NICo is ready for use by the cluster.
	NicoReadyCondition = "NicoReady"

	// SyncedCondition reports whether the desired infrastructure was reconciled.
	SyncedCondition = "Synced"

	// MachineProvisionedCondition tracks provisioning of the backing instance
	// for a machine.
	MachineProvisionedCondition = "MachineProvisioned"
)

// Condition reasons shared by both reconcilers.
const (
	// UnknownReason indicates a condition has not been reported yet.
	UnknownReason = "Unknown"

	// DeletingReason indicates the resource is being deleted.
	DeletingReason = "Deleting"

	// WaitingForIdentitySecretReason indicates the configured identity Secret is not available yet.
	WaitingForIdentitySecretReason = "WaitingForIdentitySecret"

	// IdentityConfigurationFailedReason indicates the identity configuration is invalid.
	IdentityConfigurationFailedReason = "IdentityConfigurationFailed"

	// TenantResolutionFailedReason indicates NICo tenant resolution failed.
	TenantResolutionFailedReason = "TenantResolutionFailed"

	// InfrastructureReadyReason indicates the infrastructure resource is ready.
	InfrastructureReadyReason = "InfrastructureReady"

	// SyncedReason indicates the desired infrastructure was reconciled.
	SyncedReason = "Synced"

	// NotSyncedReason indicates the desired infrastructure was not reconciled.
	NotSyncedReason = "NotSynced"

	// SyncUnknownReason indicates the reconciliation state is unknown.
	SyncUnknownReason = "SyncUnknown"

	// WaitingForClusterInfrastructureReason indicates the owning cluster scope
	// is not ready yet.
	WaitingForClusterInfrastructureReason = "WaitingForClusterInfrastructure"

	// WaitingForBootstrapDataReason indicates bootstrap data is not available yet.
	WaitingForBootstrapDataReason = "WaitingForBootstrapData"

	// BootstrapDataInvalidReason indicates bootstrap data could not be used.
	BootstrapDataInvalidReason = "BootstrapDataInvalid"

	// InstanceCreateRequestInvalidReason indicates the NICo instance create request could not be built.
	InstanceCreateRequestInvalidReason = "InstanceCreateRequestInvalid"

	// AvailabilityCheckFailedReason indicates the instance type availability check failed.
	AvailabilityCheckFailedReason = "AvailabilityCheckFailed"

	// InstanceTypeNotFoundReason indicates the configured instance type does not exist.
	InstanceTypeNotFoundReason = "InstanceTypeNotFound"

	// InstanceTypeUnavailableReason indicates the configured instance type has no available capacity.
	InstanceTypeUnavailableReason = "InstanceTypeUnavailable"

	// InstanceCreateFailedReason indicates NICo instance creation failed.
	InstanceCreateFailedReason = "InstanceCreateFailed"

	// InstanceMissingReason indicates the backing NICo instance could not be found.
	InstanceMissingReason = "InstanceMissing"

	// InstanceNotReadyReason indicates the backing NICo instance is not ready yet.
	InstanceNotReadyReason = "InstanceNotReady"

	// InstanceReadyReason indicates the backing NICo instance is ready.
	InstanceReadyReason = "InstanceReady"

	// InstanceNotFoundReason indicates that the requested import instance is not available in NICo
	InstanceNotFoundReason = "InstanceNotFound"

	// InstanceAlreadyClaimedReason indicates another NicoMachine already references the backing instance.
	InstanceAlreadyClaimedReason = "InstanceAlreadyClaimed"

	// WaitingForNicoMachinesDeletionReason indicates the cluster is waiting for all NicoMachines to be deleted.
	WaitingForNicoMachinesDeletionReason = "WaitingForNicoMachinesDeletion"

	// ControlPlanePriorityDeferredReason indicates a worker NicoMachine's instance
	// create is being held back so a control-plane NicoMachine waiting on the same
	// instance type can claim scarce capacity first.
	ControlPlanePriorityDeferredReason = "ControlPlanePriorityDeferred"
)
