// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/internal/constants"
)

type GetInstanceTypeOutput struct {
	InfiniBandDeviceName      string
	InfiniBandActiveDeviceIDs []int32
	NVLinkInterfaceCount      int32
	NVLinkActiveDeviceIDs     []int32
}

func (a *Activities) getInstanceType(ctx context.Context, orgID, siteID string, instanceTypeID uuid.UUID) (GetInstanceTypeOutput, error) {
	instanceType, err := a.forgeProvider.GetInstanceTypeByID(ctx, orgID, siteID, instanceTypeID.String())
	if err != nil {
		return GetInstanceTypeOutput{}, fmt.Errorf("failed to get instance type from forge provider: %w", err)
	}

	return parseInstanceTypeCapabilities(instanceType), nil
}

func parseInstanceTypeCapabilities(instanceType interface {
	GetMachineCapabilities() []forgesdk.MachineCapability
},
) GetInstanceTypeOutput {
	response := GetInstanceTypeOutput{}

	for _, machineCapability := range instanceType.GetMachineCapabilities() {
		activeDeviceIDs := getActiveDeviceIDs(machineCapability.GetCount(), machineCapability.GetInactiveDevices())
		deviceName := machineCapability.GetName()
		if machineCapability.GetType() == constants.MachineCapabilityTypeInfiniBand &&
			deviceName != "" &&
			response.InfiniBandDeviceName == "" {
			response.InfiniBandDeviceName = deviceName
			response.InfiniBandActiveDeviceIDs = activeDeviceIDs
		}
		if isNVLinkCapability(machineCapability) && response.NVLinkInterfaceCount == 0 {
			response.NVLinkInterfaceCount = countDeviceIDs(activeDeviceIDs)
			response.NVLinkActiveDeviceIDs = activeDeviceIDs
		}
	}

	return response
}

func getActiveDeviceIDs(deviceCount int32, inactiveDevices []int32) []int32 {
	activeDeviceIDs := make([]int32, 0, deviceCount)
	for deviceID := range deviceCount {
		// Assume the device id is active unless we find it in the inactive devices list.
		if !slices.Contains(inactiveDevices, deviceID) {
			activeDeviceIDs = append(activeDeviceIDs, deviceID)
		}
	}

	return activeDeviceIDs
}

func countDeviceIDs(deviceIDs []int32) int32 {
	var count int32
	for range deviceIDs {
		count++
	}

	return count
}

func isNVLinkCapability(machineCapability forgesdk.MachineCapability) bool {
	capabilityType := strings.TrimSpace(machineCapability.GetType())
	deviceType := strings.TrimSpace(machineCapability.GetDeviceType())

	if strings.EqualFold(capabilityType, constants.MachineCapabilityTypeNVLink) {
		return true
	}
	if strings.EqualFold(capabilityType, constants.MachineCapabilityTypeGPU) &&
		strings.EqualFold(deviceType, constants.MachineCapabilityDeviceTypeNVLink) {
		return true
	}
	if strings.Contains(strings.ToLower(capabilityType), "nvlink") ||
		strings.Contains(strings.ToLower(deviceType), "nvlink") {
		return true
	}

	return false
}
