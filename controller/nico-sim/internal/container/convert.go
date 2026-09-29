// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	nicosdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/dsx-ai-factory/nke/controller/nico-sim/internal/apierr"
)

// loadInstance reconstructs the API object from Docker labels. It intentionally
// rejects containers that belong to a different catalog bundle: the shared
// managed label predates the unified server and is not, by itself, sufficient
// proof that a container belongs to this site's backend.
func (s *Service) loadInstance(ctx context.Context, containerID string) (*nicosdk.Instance, error) {
	inspectResult, err := s.inspectContainer(ctx, containerID)
	if err != nil {
		return nil, err
	}

	labels := inspectResult.Config.Labels
	instanceID := strings.TrimSpace(labels[labelInstanceID])
	if instanceID == "" {
		return nil, apierr.ErrNotFound
	}
	if err := s.validateOwnership(labels); err != nil {
		return nil, err
	}

	instance := nicosdk.NewInstance()
	instance.SetId(instanceID)
	instance.SetName(labels[labelInstanceName])
	instance.SetSiteId(s.cfg.SiteID)
	instance.SetVpcId(s.cfg.VPCID)
	instance.SetTenantId(s.cfg.TenantID)
	instance.SetInstanceTypeId(s.cfg.InstanceTypeID)
	if machineID := labels[labelMachineID]; machineID != "" {
		instance.SetMachineId(machineID)
	}
	if ipxeScript, err := decodeLabelValue[string](labels[labelIpxeScript]); err != nil {
		return nil, fmt.Errorf("decode iPXE script label: %w", err)
	} else if ipxeScript != "" {
		instance.SetIpxeScript(ipxeScript)
	}
	if groupIDs, err := decodeLabelValue[[]string](labels[labelSSHKeyGroupIDs]); err != nil {
		return nil, fmt.Errorf("decode SSH key group IDs label: %w", err)
	} else if len(groupIDs) > 0 {
		instance.SetSshKeyGroupIds(groupIDs)
	}
	if instanceLabels := publicInstanceLabels(labels); len(instanceLabels) > 0 {
		instance.SetLabels(instanceLabels)
	}

	tpmEKCertificate, err := s.tpmEKCertificate(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("read TPM EK certificate for %s: %w", containerID, err)
	}
	instance.SetStatus(instanceStatusForContainer(inspectResult.State.Running, tpmEKCertificate, labels))
	if tpmEKCertificate != "" {
		instance.SetTpmEkCertificate(tpmEKCertificate)
	}

	if interfaces, err := decodeLabelValue[[]nicosdk.InterfaceCreateRequest](labels[labelInterfaces]); err != nil {
		return nil, fmt.Errorf("decode interfaces label: %w", err)
	} else if len(interfaces) > 0 {
		instance.SetInterfaces(instanceInterfacesFromCreateRequests(interfaces, s.firstContainerIPAddress(inspectResult)))
	} else if ipAddress := s.firstContainerIPAddress(inspectResult); ipAddress != "" || labels[labelSubnetID] != "" || labels[labelVPCPrefixID] != "" {
		instance.SetInterfaces([]nicosdk.Interface{legacyInstanceInterface(labels, ipAddress)})
	}
	if interfaces, err := decodeLabelValue[[]nicosdk.InfiniBandInterfaceCreateRequest](labels[labelInfiniBandInterfaces]); err != nil {
		return nil, fmt.Errorf("decode InfiniBand interfaces label: %w", err)
	} else if len(interfaces) > 0 {
		instance.SetInfinibandInterfaces(instanceInfiniBandInterfacesFromCreateRequests(interfaces))
	}
	if interfaces, err := decodeLabelValue[[]nicosdk.NVLinkInterfaceCreateRequest](labels[labelNVLinkInterfaces]); err != nil {
		return nil, fmt.Errorf("decode NVLink interfaces label: %w", err)
	} else if len(interfaces) > 0 {
		instance.SetNvLinkInterfaces(instanceNVLinkInterfacesFromCreateRequests(interfaces))
	}

	if created, err := time.Parse(time.RFC3339Nano, inspectResult.Created); err == nil {
		instance.SetCreated(created)
		instance.SetUpdated(created)
	}
	return instance, nil
}

func (s *Service) validateOwnership(labels map[string]string) error {
	want := map[string]string{
		labelTenantID:     s.cfg.TenantID,
		labelSiteID:       s.cfg.SiteID,
		labelVPCID:        s.cfg.VPCID,
		labelInstanceType: s.cfg.InstanceTypeID,
	}
	for label, expected := range want {
		if actual := strings.TrimSpace(labels[label]); actual != expected {
			return fmt.Errorf("%w: label %s is %q, expected %q", apierr.ErrOwnershipConflict, label, actual, expected)
		}
	}
	interfaces, err := decodeLabelValue[[]nicosdk.InterfaceCreateRequest](labels[labelInterfaces])
	if err != nil {
		return fmt.Errorf("%w: decode interfaces label: %v", apierr.ErrOwnershipConflict, err)
	}
	for _, iface := range interfaces {
		if iface.GetSubnetId() != s.cfg.SubnetID || iface.GetVpcPrefixId() != "" {
			return fmt.Errorf("%w: interface does not belong to configured subnet %s", apierr.ErrOwnershipConflict, s.cfg.SubnetID)
		}
	}
	groupIDs, err := decodeLabelValue[[]string](labels[labelSSHKeyGroupIDs])
	if err != nil {
		return fmt.Errorf("%w: decode ssh key groups label: %v", apierr.ErrOwnershipConflict, err)
	}
	for _, groupID := range groupIDs {
		if groupID != s.cfg.SSHKeyGroupID {
			return fmt.Errorf("%w: ssh key group %s does not belong to configured container site", apierr.ErrOwnershipConflict, groupID)
		}
	}
	return nil
}

func instanceStatusForContainer(running bool, tpmEKCertificate string, labels map[string]string) nicosdk.InstanceStatus {
	attestationMethod := labels[labelSpireAttestationMethod]
	if attestationMethod == "" {
		// Treat legacy containers without a pinned method as TPM so a missing
		// EK certificate cannot make them appear ready after the default changes.
		attestationMethod = "tpm"
	}
	switch {
	case !running:
		return nicosdk.InstanceStatus("Error")
	case attestationMethod == "tpm" && strings.TrimSpace(tpmEKCertificate) == "":
		return nicosdk.InstanceStatus("Pending")
	default:
		return nicosdk.InstanceStatus("Ready")
	}
}

func encodeLabelValue[T any](value T) string {
	payload, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("encode container label value: %v", err))
	}
	return base64.StdEncoding.EncodeToString(payload)
}

func decodeLabelValue[T any](value string) (T, error) {
	var decoded T
	if strings.TrimSpace(value) == "" {
		return decoded, nil
	}
	payload, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return decoded, err
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return decoded, err
	}
	return decoded, nil
}

func instanceInterfacesFromCreateRequests(requests []nicosdk.InterfaceCreateRequest, ipAddress string) []nicosdk.Interface {
	interfaces := make([]nicosdk.Interface, 0, len(requests))
	for _, request := range requests {
		iface := nicosdk.NewInterface()
		if subnetID := request.GetSubnetId(); subnetID != "" {
			iface.SetSubnetId(subnetID)
		}
		if vpcPrefixID := request.GetVpcPrefixId(); vpcPrefixID != "" {
			iface.SetVpcPrefixId(vpcPrefixID)
		}
		if requestedIP := request.GetIpAddress(); requestedIP != "" {
			iface.SetRequestedIpAddress(requestedIP)
		}
		if physical, ok := request.GetIsPhysicalOk(); ok && physical != nil {
			iface.SetIsPhysical(*physical)
		}
		if device := request.GetDevice(); device != "" {
			iface.SetDevice(device)
		}
		if deviceInstance, ok := request.GetDeviceInstanceOk(); ok && deviceInstance != nil {
			iface.SetDeviceInstance(*deviceInstance)
		}
		if virtualFunctionID, ok := request.GetVirtualFunctionIdOk(); ok && virtualFunctionID != nil {
			iface.SetVirtualFunctionId(*virtualFunctionID)
		}
		if ipAddress != "" {
			iface.SetIpAddresses([]string{ipAddress})
		}
		interfaces = append(interfaces, *iface)
	}
	return interfaces
}

func legacyInstanceInterface(labels map[string]string, ipAddress string) nicosdk.Interface {
	iface := nicosdk.NewInterface()
	if subnetID := labels[labelSubnetID]; subnetID != "" {
		iface.SetSubnetId(subnetID)
	}
	if vpcPrefixID := labels[labelVPCPrefixID]; vpcPrefixID != "" {
		iface.SetVpcPrefixId(vpcPrefixID)
	}
	if ipAddress != "" {
		iface.SetIpAddresses([]string{ipAddress})
	}
	return *iface
}

func instanceInfiniBandInterfacesFromCreateRequests(requests []nicosdk.InfiniBandInterfaceCreateRequest) []nicosdk.InfiniBandInterface {
	interfaces := make([]nicosdk.InfiniBandInterface, 0, len(requests))
	for _, request := range requests {
		iface := nicosdk.NewInfiniBandInterface()
		if partitionID := request.GetPartitionId(); partitionID != "" {
			iface.SetPartitionId(partitionID)
		}
		if device := request.GetDevice(); device != "" {
			iface.SetDevice(device)
		}
		if deviceInstance, ok := request.GetDeviceInstanceOk(); ok && deviceInstance != nil {
			iface.SetDeviceInstance(*deviceInstance)
		}
		if physical, ok := request.GetIsPhysicalOk(); ok && physical != nil {
			iface.SetIsPhysical(*physical)
		}
		if virtualFunctionID, ok := request.GetVirtualFunctionIdOk(); ok && virtualFunctionID != nil {
			iface.SetVirtualFunctionId(*virtualFunctionID)
		}
		interfaces = append(interfaces, *iface)
	}
	return interfaces
}

func instanceNVLinkInterfacesFromCreateRequests(requests []nicosdk.NVLinkInterfaceCreateRequest) []nicosdk.NVLinkInterface {
	interfaces := make([]nicosdk.NVLinkInterface, 0, len(requests))
	for _, request := range requests {
		iface := nicosdk.NewNVLinkInterface()
		if partitionID := request.GetNvLinklogicalPartitionId(); partitionID != "" {
			iface.SetNvLinkLogicalPartitionId(partitionID)
		}
		if deviceInstance, ok := request.GetDeviceInstanceOk(); ok && deviceInstance != nil {
			iface.SetDeviceInstance(*deviceInstance)
		}
		interfaces = append(interfaces, *iface)
	}
	return interfaces
}

func publicInstanceLabels(labels map[string]string) map[string]string {
	public := make(map[string]string)
	for key, value := range labels {
		switch key {
		case labelManaged, labelInstanceID, labelInstanceName, labelSiteID, labelVPCID,
			labelTenantID, labelInstanceType, labelSubnetID, labelVPCPrefixID, labelMachineID,
			labelSpireAttestationMethod,
			labelIpxeScript, labelSSHKeyGroupIDs, labelInterfaces, labelInfiniBandInterfaces,
			labelNVLinkInterfaces:
			continue
		default:
			public[key] = value
		}
	}
	if len(public) == 0 {
		return nil
	}
	return public
}
