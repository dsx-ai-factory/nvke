// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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

// Power reboots, stops and starts a VM without deleting it.
type Power interface {
	Reboot(ctx context.Context, machineID string) error
	// Stop shuts the VM down but keeps its disk, TPM state and definition so
	// Start can boot it again.
	Stop(ctx context.Context, machineID string) error
	Start(ctx context.Context, machineID string) error
}

// Runtime abstracts the hypervisor backend used to manage VMs.
type Runtime interface {
	Power
	EnsureBridge(ctx context.Context, bridgeName, bridgeCIDR string) error
	CreateAndStart(ctx context.Context, spec InstanceSpec) (string, error)
	Delete(ctx context.Context, machineID string) error
	IsRunning(ctx context.Context, machineID string) (bool, error)
	GetState(ctx context.Context, machineID string) (MachineState, error)
	SupportsTCPProbe() bool

	// ListMachines returns the IDs of all VMs known to the backend.
	// Used at startup to detect orphaned VMs that exist in the hypervisor
	// but not in the database.
	ListMachines(ctx context.Context) ([]string, error)

	// LookupDHCPLease returns the IPv4 address the hypervisor's DHCP server
	// has leased to the given MAC on the named network. Used only in
	// libvirt-network mode, where libvirt's own dnsmasq assigns addresses.
	// Return semantics:
	//   (ip, true, nil)    — lease found
	//   ("", false, nil)   — no lease yet (caller retries) or backend unsupported
	//   ("", false, err)   — lookup command failed
	LookupDHCPLease(ctx context.Context, network, mac string) (string, bool, error)

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
	NetworkName string // when set, attach to this libvirt network instead of BridgeName
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
