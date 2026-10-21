// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"fmt"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// ValidateNodePoolPayload validates nodePool's LaunchLayer payload content:
// instanceType is required, and at most one of hostOSProfile/vmProfile may
// be set (CAPLL itself enforces this with a CEL rule, but NVKE's own error
// message is better UX than admission rejection).
func (*Client) ValidateNodePoolPayload(nodePool db.NodePool) error {
	provider, err := nodePool.LaunchLayerProvider()
	if err != nil {
		return err
	}
	if err := validateLaunchLayerMachineFields(provider.InstanceType, provider.HostOSProfile, provider.VMProfile, ""); err != nil {
		return err
	}
	return validateSecurityGroupAttachments(provider.SecurityGroups, provider.SecurityGroupRefs, "launchLayer.")
}

func validateSecurityGroupAttachments(groups db.LaunchLayerSecurityGroups, refs db.LaunchLayerSecurityGroupRefs, fieldPrefix string) error {
	if len(groups.PrimaryNetworkInterface) > 0 && len(refs.PrimaryNetworkInterface) > 0 {
		return fmt.Errorf("%ssecurityGroups and %ssecurityGroupRefs are mutually exclusive on primaryNetworkInterface", fieldPrefix, fieldPrefix)
	}
	if len(groups.GPUNetworkInterface) > 0 && len(refs.GPUNetworkInterface) > 0 {
		return fmt.Errorf("%ssecurityGroups and %ssecurityGroupRefs are mutually exclusive on gpuNetworkInterface", fieldPrefix, fieldPrefix)
	}
	for _, ids := range [][]string{groups.PrimaryNetworkInterface, groups.GPUNetworkInterface} {
		for _, id := range ids {
			if id == "" {
				return fmt.Errorf("%ssecurityGroups must not contain empty IDs", fieldPrefix)
			}
		}
	}
	return nil
}

// validateLaunchLayerMachineFields validates the fields shared by node-pool
// and control-plane LaunchLayer payloads. fieldPrefix names the request
// field this error maps back to (e.g. "" for a node pool, "controlPlaneConfig."
// for a control plane).
func validateLaunchLayerMachineFields(instanceType, hostOSProfile, vmProfile, fieldPrefix string) error {
	if instanceType == "" {
		return fmt.Errorf("%sinstanceType is required", fieldPrefix)
	}
	if hostOSProfile != "" && vmProfile != "" {
		return fmt.Errorf("at most one of %shostOSProfile or %svmProfile may be provided", fieldPrefix, fieldPrefix)
	}
	return nil
}
