// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"net"
	"strings"

	nicosdk "github.com/NVIDIA/infra-controller/rest-api/sdk/standard"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/state"
)

func toSDKInstance(row state.Instance) nicosdk.Instance {
	instance := nicosdk.NewInstance()
	instance.SetId(row.ID)
	instance.SetName(row.Name)
	instance.SetTenantId(row.TenantID)
	instance.SetSiteId(row.SiteID)
	instance.SetInstanceTypeId(row.InstanceTypeID)
	instance.SetVpcId(row.VPCID)
	if row.MachineID != "" {
		instance.SetMachineId(row.MachineID)
	} else {
		instance.SetMachineIdNil()
	}
	if row.IpxeScript != "" {
		instance.SetIpxeScript(row.IpxeScript)
	}
	if row.UserData != "" {
		instance.SetUserData(row.UserData)
	}
	labels, _ := row.Labels()
	instance.SetLabels(labels)
	setInstanceBackendLabel(instance, row.Backend)
	groupIDs, _ := row.SSHKeyGroupIDs()
	instance.SetSshKeyGroupIds(groupIDs)
	instance.SetStatus(nicosdk.InstanceStatus(instanceStatus(row.Status)))
	instance.SetPhoneHomeEnabled(row.PhoneHomeEnabled)
	if row.TpmEkCertificate != "" {
		instance.SetTpmEkCertificate(row.TpmEkCertificate)
	}
	instance.SetCreated(row.CreatedAt)
	instance.SetUpdated(row.UpdatedAt)

	exposeIPs := row.VM != nil && (row.VM.ObservedState == state.ObservedStateReady || row.VM.ObservedState == state.ObservedStateRebooting)
	ifaces := make([]nicosdk.Interface, 0, len(row.Interfaces))
	for _, ifaceRow := range row.Interfaces {
		iface := nicosdk.NewInterface()
		iface.SetId(ifaceRow.ID)
		iface.SetInstanceId(ifaceRow.InstanceID)
		if ifaceRow.SubnetID != "" {
			iface.SetSubnetId(ifaceRow.SubnetID)
		}
		if ifaceRow.VPCPrefixID != "" {
			iface.SetVpcPrefixId(ifaceRow.VPCPrefixID)
		}
		iface.SetStatus(nicosdk.InterfaceStatus(titleCase(ifaceRow.Status)))
		if exposeIPs {
			ips, _ := ifaceRow.IPAddresses()
			iface.SetIpAddresses(ips)
		} else {
			iface.SetIpAddresses([]string{})
		}
		ifaces = append(ifaces, *iface)
	}
	instance.SetInterfaces(ifaces)
	return *instance
}

// toSDKTenant maps a stored tenant to the SDK Tenant shape. CAPNICo resolves
// its tenant ID from the "id" field of this response before creating instances.
func toSDKTenant(row state.TenantRow) nicosdk.Tenant {
	item := nicosdk.NewTenant()
	item.SetId(row.ID)
	item.SetOrg(row.OrgID)
	item.SetOrgDisplayName(row.DisplayName)
	return *item
}

func toSDKSite(row state.SiteRow, orgID string) nicosdk.Site {
	item := nicosdk.NewSite()
	item.SetId(row.ID)
	item.SetName(row.Name)
	item.SetOrg(orgID)
	item.SetIsOnline(true)
	caps := nicosdk.NewSiteCapabilities()
	caps.SetNativeNetworking(row.CapabilitiesNativeNetwork)
	caps.SetNetworkSecurityGroup(false)
	item.SetCapabilities(*caps)
	item.SetCreated(row.CreatedAt)
	item.SetUpdated(row.UpdatedAt)
	return *item
}

func toSDKVPC(row state.VPCRow) nicosdk.VPC {
	item := nicosdk.NewVPC()
	item.SetId(row.ID)
	item.SetName(row.Name)
	item.SetSiteId(row.SiteID)
	item.SetTenantId(row.TenantID)
	item.SetStatus("Ready")
	return *item
}

func toSDKSubnet(row state.SubnetRow) nicosdk.Subnet {
	item := nicosdk.NewSubnet()
	item.SetId(row.ID)
	item.SetName(row.Name)
	item.SetSiteId(row.SiteID)
	item.SetVpcId(row.VPCID)
	item.SetPrefixLength(int32(row.PrefixLength))
	ipv4Prefix := row.CIDR
	if _, subnet, err := net.ParseCIDR(row.CIDR); err == nil {
		ipv4Prefix = subnet.IP.String()
	}
	item.SetIpv4Prefix(ipv4Prefix)
	item.SetStatus("Ready")
	return *item
}

func toSDKVPCPrefix(row state.VPCPrefixRow) nicosdk.VpcPrefix {
	item := nicosdk.NewVpcPrefix()
	item.SetId(row.ID)
	item.SetName(row.Name)
	item.SetSiteId(row.SiteID)
	item.SetVpcId(row.VPCID)
	item.SetPrefixLength(int32(row.PrefixLength))
	item.SetPrefix(row.Prefix)
	item.SetStatus("Ready")
	return *item
}

func toSDKInstanceType(row state.InstanceTypeRow, used int32) nicosdk.InstanceType {
	item := nicosdk.NewInstanceType()
	item.SetId(row.ID)
	item.SetName(row.Name)
	item.SetSiteId(row.SiteID)

	stats := nicosdk.NewInstanceTypeAllocationStats()
	unused := fakeInstanceTypeTotal - used
	if unused < 0 {
		unused = 0
	}
	stats.SetTotal(fakeInstanceTypeTotal)
	stats.SetUsed(used)
	stats.SetUnused(unused)
	stats.SetUnusedUsable(unused)
	item.SetAllocationStats(*stats)

	ibCap := nicosdk.NewMachineCapability()
	ibCap.SetType("InfiniBand")
	ibCap.SetName(fakeIBCapabilityName)
	ibCap.SetCount(fakeIBCapabilityCount)
	ibCap.SetInactiveDevices([]uint32{})
	item.SetMachineCapabilities([]nicosdk.MachineCapability{*ibCap})
	item.SetCreated(row.CreatedAt)
	item.SetUpdated(row.UpdatedAt)
	return *item
}

func toSDKSSHKeyGroup(row state.SSHKeyGroupRow, siteID string) nicosdk.SshKeyGroup {
	item := nicosdk.NewSshKeyGroup()
	item.SetId(row.ID)
	item.SetName(row.Name)
	item.SetStatus("Synced")

	siteSummary := nicosdk.NewSiteSummary()
	siteSummary.SetId(siteID)
	siteSummary.SetName(siteID)
	assoc := nicosdk.NewSshKeyGroupSiteAssociation()
	assoc.SetSite(*siteSummary)
	assoc.SetStatus("Synced")
	item.SetSiteAssociations([]nicosdk.SshKeyGroupSiteAssociation{*assoc})
	return *item
}

func toSDKIPBlock(row state.IPBlockRow) nicosdk.IpBlock {
	item := nicosdk.NewIpBlock()
	item.SetId(row.ID)
	item.SetName(row.Name)
	item.SetSiteId(row.SiteID)
	item.SetStatus("Ready")
	return *item
}

func toSDKInfiniBandPartition(row state.InfiniBandPartitionRow) nicosdk.InfiniBandPartition {
	item := nicosdk.NewInfiniBandPartition()
	item.SetId(row.ID)
	item.SetName(row.Name)
	item.SetSiteId(row.SiteID)
	item.SetStatus("Ready")
	return *item
}

// titleCase converts "provisioning" → "Provisioning", "ready" → "Ready", etc.
// The Forge SDK v0.0.14 enum validation expects title-cased status values.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// instanceStatus maps internal DB status to SDK InstanceStatus enum values.
// The SDK enum is title-cased and uses different names for some states.
func instanceStatus(dbStatus string) string {
	switch dbStatus {
	case "running":
		return "Configuring" // VM started, cloud-init running, SSH not yet ready
	case "deleting":
		return "Terminating" // SDK uses "Terminating", not "Deleting"
	case "stopped", "suspended":
		return "Error" // Forge SDK has no stopped/suspended; detail in ErrorReason
	default:
		return titleCase(dbStatus)
	}
}
