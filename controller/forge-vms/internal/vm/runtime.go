// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

// Package vm defines the Runtime interface shared by all VM backends.
package vm

import "context"

// MachineState describes the observed hypervisor state of a VM.
type MachineState string

const (
	MachineStateRunning  MachineState = "running"
	MachineStateShutOff  MachineState = "shutoff"
	MachineStateCrashed  MachineState = "crashed"
	MachineStatePaused   MachineState = "paused"
	MachineStateNotFound MachineState = "notfound"
)

// Runtime abstracts the hypervisor backend used to manage VMs.
type Runtime interface {
	EnsureBridge(ctx context.Context, bridgeName, bridgeCIDR string) error
	CreateAndStart(ctx context.Context, spec InstanceSpec) (string, error)
	Reboot(ctx context.Context, machineID string) error
	Delete(ctx context.Context, machineID string) error
	IsRunning(ctx context.Context, machineID string) (bool, error)
	GetState(ctx context.Context, machineID string) (MachineState, error)
	SupportsTCPProbe() bool

	// ListMachines returns the IDs of all VMs known to the backend.
	// Used at startup to detect orphaned VMs that exist in the hypervisor
	// but not in the database.
	ListMachines(ctx context.Context) ([]string, error)

	// TPMEKCert returns the base64-encoded TPM Endorsement Key certificate
	// for the given machine. Return semantics:
	//   ("", nil)   — no TPM configured for this backend
	//   (cert, nil) — success
	//   ("", err)   — TPM present but cert could not be read (caller should retry)
	TPMEKCert(ctx context.Context, machineID string) (string, error)
}

// InstanceSpec describes a VM to be created.
type InstanceSpec struct {
	InstanceID  string
	DomainName  string
	BridgeName  string
	VCPUs       int
	MemoryMiB   int
	DiskPath    string
	SeedISOPath string
	NICMAC      string
	Arch        string // "x86_64" or "aarch64"
	UEFI        bool   // when true, uses UEFI firmware (OVMF for x86_64, AAVMF for aarch64)
}

// RuntimeOptions holds configuration used to select and initialise a Runtime.
type RuntimeOptions struct {
	Backend        string
	LibvirtURI     string
	QEMUSystemPath string
	InstancesDir   string
}
