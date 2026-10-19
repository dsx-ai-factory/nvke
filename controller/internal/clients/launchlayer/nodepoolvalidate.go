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
	return validateLaunchLayerMachineFields(provider.InstanceType, provider.HostOSProfile, provider.VMProfile, "")
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
