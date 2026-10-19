// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// ValidateControlPlaneConfigPayload validates spec's LaunchLayer payload
// content: same rules as ValidateNodePoolPayload — control-plane nodes are
// still LaunchLayer machines with the same field set.
func (*Client) ValidateControlPlaneConfigPayload(spec db.ControlPlaneConfigSpec) error {
	provider, err := spec.LaunchLayerProvider()
	if err != nil {
		return err
	}
	return validateLaunchLayerMachineFields(provider.InstanceType, provider.HostOSProfile, provider.VMProfile, "controlPlaneConfig.")
}
