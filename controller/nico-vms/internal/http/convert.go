// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"fmt"
	"net"
	nethttp "net/http"
	"strings"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/nico-vms/internal/state"
)

func toSDKInstance(row state.InstanceWithInterfaces) forgesdk.Instance {
	instance := forgesdk.NewInstance()
	instance.SetId(row.Instance.ID)
	instance.SetName(row.Instance.Name)
	instance.SetTenantId(row.Instance.TenantID)
	instance.SetSiteId(row.Instance.SiteID)
	instance.SetInstanceTypeId(row.Instance.InstanceTypeID)
	instance.SetVpcId(row.Instance.VPCID)
	if row.Instance.MachineID != "" {
		instance.SetMachineId(row.Instance.MachineID)
	} else {
		instance.SetMachineIdNil()
	}
	if row.Instance.IpxeScript != "" {
		instance.SetIpxeScript(row.Instance.IpxeScript)
	}
	if row.Instance.UserData != "" {
		instance.SetUserData(row.Instance.UserData)
	}
	labels, _ := row.Instance.Labels()
	instance.SetLabels(labels)
	groupIDs, _ := row.Instance.SSHKeyGroupIDs()
	instance.SetSshKeyGroupIds(groupIDs)
	instance.SetStatus(forgesdk.InstanceStatus(instanceStatus(row.Instance.Status)))
	instance.SetPhoneHomeEnabled(row.Instance.PhoneHomeEnabled)
	if row.Instance.TpmEkCertificate != "" {
		instance.SetTpmEkCertificate(row.Instance.TpmEkCertificate)
	}
	instance.SetCreated(row.Instance.CreatedAt)
	instance.SetUpdated(row.Instance.UpdatedAt)

	exposeIPs := row.Instance.ObservedState == state.ObservedStateReady || row.Instance.ObservedState == state.ObservedStateRebooting
	ifaces := make([]forgesdk.Interface, 0, len(row.Interfaces))
	for _, ifaceRow := range row.Interfaces {
		iface := forgesdk.NewInterface()
		iface.SetId(ifaceRow.ID)
		iface.SetInstanceId(ifaceRow.InstanceID)
		if ifaceRow.SubnetID != "" {
			iface.SetSubnetId(ifaceRow.SubnetID)
		}
		if ifaceRow.VPCPrefixID != "" {
			iface.SetVpcPrefixId(ifaceRow.VPCPrefixID)
		}
		iface.SetStatus(forgesdk.InterfaceStatus(titleCase(ifaceRow.Status)))
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
func toSDKTenant(row state.TenantRow) forgesdk.Tenant {
	item := forgesdk.NewTenant()
	item.SetId(row.ID)
	item.SetOrg(row.OrgID)
	item.SetOrgDisplayName(row.DisplayName)
	return *item
}

func toSDKSite(row state.SiteRow) forgesdk.Site {
	item := forgesdk.NewSite()
	item.SetId(row.ID)
	item.SetName(row.Name)
	caps := forgesdk.NewSiteCapabilities()
	caps.SetNativeNetworking(row.CapabilitiesNativeNetwork)
	item.SetCapabilities(*caps)
	item.SetCreated(row.CreatedAt)
	item.SetUpdated(row.UpdatedAt)
	return *item
}

func toSDKVPC(row state.VPCRow) forgesdk.VPC {
	item := forgesdk.NewVPC()
	item.SetId(row.ID)
	item.SetName(row.Name)
	item.SetSiteId(row.SiteID)
	item.SetTenantId(row.TenantID)
	item.SetStatus("Ready")
	return *item
}

func toSDKSubnet(row state.SubnetRow) forgesdk.Subnet {
	item := forgesdk.NewSubnet()
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

func toSDKVPCPrefix(row state.VPCPrefixRow) forgesdk.VpcPrefix {
	item := forgesdk.NewVpcPrefix()
	item.SetId(row.ID)
	item.SetName(row.Name)
	item.SetSiteId(row.SiteID)
	item.SetVpcId(row.VPCID)
	item.SetPrefixLength(int32(row.PrefixLength))
	item.SetPrefix(row.Prefix)
	item.SetStatus("Ready")
	return *item
}

func toSDKInstanceType(row state.InstanceTypeRow, used int32) forgesdk.InstanceType {
	item := forgesdk.NewInstanceType()
	item.SetId(row.ID)
	item.SetName(row.Name)
	item.SetSiteId(row.SiteID)

	stats := forgesdk.NewInstanceTypeAllocationStats()
	total := fakeInstanceTypeTotal
	unused := total - used
	if unused < 0 {
		unused = 0
	}
	stats.SetTotal(total)
	stats.SetUsed(used)
	stats.SetUnused(unused)
	stats.SetUnusedUsable(unused)
	item.SetAllocationStats(*stats)

	ibCap := forgesdk.NewMachineCapability()
	ibCap.SetType("InfiniBand")
	ibCap.SetName(fakeIBCapabilityName)
	ibCap.SetCount(fakeIBCapabilityCount)
	ibCap.SetInactiveDevices([]int32{})
	item.SetMachineCapabilities([]forgesdk.MachineCapability{*ibCap})
	item.SetCreated(row.CreatedAt)
	item.SetUpdated(row.UpdatedAt)
	return *item
}

func toSDKSSHKeyGroup(row state.SSHKeyGroupRow, siteID string) forgesdk.SshKeyGroup {
	item := forgesdk.NewSshKeyGroup()
	item.SetId(row.ID)
	item.SetName(row.Name)
	item.SetStatus("Synced")

	siteSummary := forgesdk.NewSiteSummary()
	siteSummary.SetId(siteID)
	siteSummary.SetName(siteID)
	assoc := forgesdk.NewSshKeyGroupSiteAssociation()
	assoc.SetSite(*siteSummary)
	assoc.SetStatus("Synced")
	item.SetSiteAssociations([]forgesdk.SshKeyGroupSiteAssociation{*assoc})
	return *item
}

func toSDKIPBlock(row state.IPBlockRow) forgesdk.IpBlock {
	item := forgesdk.NewIpBlock()
	item.SetId(row.ID)
	item.SetName(row.Name)
	item.SetSiteId(row.SiteID)
	item.SetStatus("Ready")
	return *item
}

func toSDKInfiniBandPartition(row state.InfiniBandPartitionRow) forgesdk.InfiniBandPartition {
	item := forgesdk.NewInfiniBandPartition()
	item.SetId(row.ID)
	item.SetName(row.Name)
	item.SetSiteId(row.SiteID)
	item.SetStatus("Ready")
	return *item
}

// instanceWithConsoleURL sets the SerialConsoleUrl on the SDK instance
// using the SSH hostname derived from the HTTP request's Host header.
func (s *Server) instanceWithConsoleURL(inst forgesdk.Instance, r *nethttp.Request) forgesdk.Instance {
	if s.consoleSSHPort > 0 {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host == "" {
			host = "localhost"
		}
		inst.SetSerialConsoleUrl(fmt.Sprintf("ssh://%s@%s:%d", inst.GetId(), host, s.consoleSSHPort))
	}
	return inst
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
