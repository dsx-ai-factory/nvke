// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

const (
	// ReadyCondition is the condition type that the Cluster API contract reads
	// on an infrastructure object.
	ReadyCondition = "Ready"
)

const (
	// InventoryValidReason means the inventory passed validation.
	InventoryValidReason = "InventoryValid"

	// InventoryInvalidReason means the inventory is not usable. The message
	// names the problem, for example a repeated hostname or IP address.
	InventoryInvalidReason = "InventoryInvalid"

	// HostAllocatedReason means the machine holds a host.
	HostAllocatedReason = "HostAllocated"

	// NoHostAvailableReason means no host of the inventory is free for this
	// machine. Add a matching host to the inventory.
	NoHostAvailableReason = "NoHostAvailable"

	// HostNotFoundReason means the machine holds a host that the inventory no
	// longer describes. The provider keeps the binding: a node still runs on
	// that host.
	HostNotFoundReason = "HostNotFound"

	// DeletionNotSupportedReason means the bootstrap-only provider cannot make
	// a running host safe for reuse, so it blocks deletion after host allocation.
	DeletionNotSupportedReason = "DeletionNotSupported"

	// WaitingForOwnerReason means the Cluster API machine controller has not
	// yet set the owner reference on this object.
	WaitingForOwnerReason = "WaitingForOwner"

	// WaitingForClusterReason means the ProvisionedCluster of this machine
	// does not exist yet.
	WaitingForClusterReason = "WaitingForCluster"

	// HostsClaimedElsewhereReason means every host this machine could use is
	// locked by another cluster. The inventories overlap: two clusters list the
	// same physical machine, and the other one got there first.
	HostsClaimedElsewhereReason = "HostsClaimedElsewhere"
)
