// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package state

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

const (
	DesiredStatePresent = "present"
	DesiredStateAbsent  = "absent"

	ObservedStateNew          = "new"
	ObservedStateProvisioning = "provisioning"
	ObservedStateRunning      = "running"
	ObservedStateReady        = "ready"
	ObservedStateRebooting    = "rebooting"
	ObservedStateDeleting     = "deleting"
	ObservedStateDeleted      = "deleted"
	ObservedStateStopped      = "stopped"
	ObservedStateError        = "error"
)

type TenantRow struct {
	bun.BaseModel `bun:"table:tenants"`
	ID            string `bun:"id,pk"`
	OrgID         string `bun:"org_id,notnull"`
	DisplayName   string `bun:"display_name,notnull"`
}

type SiteRow struct {
	bun.BaseModel             `bun:"table:sites"`
	ID                        string    `bun:"id,pk"`
	Name                      string    `bun:"name,notnull"`
	TenantID                  string    `bun:"tenant_id,notnull"`
	CapabilitiesNativeNetwork bool      `bun:"capabilities_native_networking,notnull"`
	CreatedAt                 time.Time `bun:"created_at,notnull"`
	UpdatedAt                 time.Time `bun:"updated_at,notnull"`
}

type VPCRow struct {
	bun.BaseModel `bun:"table:vpcs"`
	ID            string `bun:"id,pk"`
	Name          string `bun:"name,notnull"`
	TenantID      string `bun:"tenant_id,notnull"`
	SiteID        string `bun:"site_id,notnull"`
}

type SubnetRow struct {
	bun.BaseModel `bun:"table:subnets"`
	ID            string `bun:"id,pk"`
	Name          string `bun:"name,notnull"`
	VPCID         string `bun:"vpc_id,notnull"`
	SiteID        string `bun:"site_id,notnull"`
	CIDR          string `bun:"cidr,notnull"`
	PrefixLength  int    `bun:"prefix_length,notnull"`
}

type VPCPrefixRow struct {
	bun.BaseModel `bun:"table:vpc_prefixes"`
	ID            string `bun:"id,pk"`
	Name          string `bun:"name,notnull"`
	VPCID         string `bun:"vpc_id,notnull"`
	SiteID        string `bun:"site_id,notnull"`
	PrefixLength  int    `bun:"prefix_length,notnull"`
	Prefix        string `bun:"prefix,notnull"`
}

type InstanceTypeRow struct {
	bun.BaseModel `bun:"table:instance_types"`
	ID            string    `bun:"id,pk"`
	Name          string    `bun:"name,notnull"`
	SiteID        string    `bun:"site_id,notnull"`
	GPUCount      int       `bun:"gpu_count,notnull"`
	CPUCount      int       `bun:"cpu_count,notnull"`
	MemoryGiB     int       `bun:"memory_gib,notnull"`
	DiskGiB       int       `bun:"disk_gib,notnull,default:0"`
	CreatedAt     time.Time `bun:"created_at,notnull"`
	UpdatedAt     time.Time `bun:"updated_at,notnull"`
}

type SSHKeyGroupRow struct {
	bun.BaseModel `bun:"table:ssh_key_groups"`
	ID            string `bun:"id,pk"`
	Name          string `bun:"name,notnull"`
	SiteIDsJSON   string `bun:"site_ids_json,notnull"`
}

func (r SSHKeyGroupRow) SiteIDs() ([]string, error) {
	var out []string
	if err := json.Unmarshal([]byte(r.SiteIDsJSON), &out); err != nil {
		return nil, fmt.Errorf("failed to decode site_ids_json for %s: %w", r.ID, err)
	}
	return out, nil
}

type IPBlockRow struct {
	bun.BaseModel `bun:"table:ip_blocks"`
	ID            string `bun:"id,pk"`
	Name          string `bun:"name,notnull"`
	SiteID        string `bun:"site_id,notnull"`
}

type InfiniBandPartitionRow struct {
	bun.BaseModel `bun:"table:infiniband_partitions"`
	ID            string `bun:"id,pk"`
	Name          string `bun:"name,notnull"`
	SiteID        string `bun:"site_id,notnull"`
}

type InstanceRow struct {
	bun.BaseModel      `bun:"table:instances"`
	ID                 string     `bun:"id,pk"`
	Name               string     `bun:"name,notnull"`
	TenantID           string     `bun:"tenant_id,notnull"`
	SiteID             string     `bun:"site_id,notnull"`
	InstanceTypeID     string     `bun:"instance_type_id,notnull"`
	VPCID              string     `bun:"vpc_id,notnull"`
	MachineID          string     `bun:"machine_id"`
	IpxeScript         string     `bun:"ipxe_script"`
	UserData           string     `bun:"user_data"`
	LabelsJSON         string     `bun:"labels_json,notnull"`
	SSHKeyGroupIDsJSON string     `bun:"ssh_key_group_ids_json,notnull"`
	DesiredState       string     `bun:"desired_state,notnull"`
	ObservedState      string     `bun:"observed_state,notnull"`
	Status             string     `bun:"status,notnull"`
	ErrorReason        string     `bun:"error_reason"`
	RebootRequested    bool       `bun:"reboot_requested,notnull"`
	PhoneHomeEnabled   bool       `bun:"phone_home_enabled,notnull"`
	PhonedHomeAt       *time.Time `bun:"phoned_home_at"`
	TpmEkCertificate   string     `bun:"tpm_ek_certificate"`
	CreatedAt          time.Time  `bun:"created_at,notnull"`
	UpdatedAt          time.Time  `bun:"updated_at,notnull"`
}

// UsesDHCP returns true when the instance gets its IP via DHCP rather than
// static assignment from IPAM. Derived from whether an iPXE script is present —
// iPXE instances boot production images that use DHCP from dnsmasq.
func (r InstanceRow) UsesDHCP() bool {
	return r.IpxeScript != ""
}

func (r InstanceRow) Labels() (map[string]string, error) {
	out := map[string]string{}
	if r.LabelsJSON == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(r.LabelsJSON), &out); err != nil {
		return nil, fmt.Errorf("failed to decode labels_json for %s: %w", r.ID, err)
	}
	return out, nil
}

func (r InstanceRow) SSHKeyGroupIDs() ([]string, error) {
	var out []string
	if r.SSHKeyGroupIDsJSON == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(r.SSHKeyGroupIDsJSON), &out); err != nil {
		return nil, fmt.Errorf("failed to decode ssh_key_group_ids_json for %s: %w", r.ID, err)
	}
	return out, nil
}

type InstanceInterfaceRow struct {
	bun.BaseModel   `bun:"table:instance_interfaces"`
	ID              string    `bun:"id,pk"`
	InstanceID      string    `bun:"instance_id,notnull"`
	SubnetID        string    `bun:"subnet_id"`
	VPCPrefixID     string    `bun:"vpc_prefix_id"`
	MACAddress      string    `bun:"mac_address"`
	IPAddressesJSON string    `bun:"ip_addresses_json,notnull"`
	Status          string    `bun:"status,notnull"`
	CreatedAt       time.Time `bun:"created_at,notnull"`
	UpdatedAt       time.Time `bun:"updated_at,notnull"`
}

func (r InstanceInterfaceRow) IPAddresses() ([]string, error) {
	var out []string
	if r.IPAddressesJSON == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(r.IPAddressesJSON), &out); err != nil {
		return nil, fmt.Errorf("failed to decode ip_addresses_json for %s: %w", r.ID, err)
	}
	return out, nil
}

type IPAllocationRow struct {
	bun.BaseModel `bun:"table:ip_allocations"`
	ID            int64     `bun:"id,pk,autoincrement"`
	ScopeType     string    `bun:"scope_type,notnull"`
	ScopeID       string    `bun:"scope_id,notnull"`
	IPAddress     string    `bun:"ip_address,notnull"`
	InstanceID    string    `bun:"instance_id,notnull"`
	InterfaceID   string    `bun:"interface_id,notnull"`
	CreatedAt     time.Time `bun:"created_at,notnull"`
}

type InstanceWithInterfaces struct {
	Instance   InstanceRow
	Interfaces []InstanceInterfaceRow
}

type Page struct {
	Number int
	Size   int
}
