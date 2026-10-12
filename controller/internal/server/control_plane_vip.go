// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	controlPlaneVIPRequiredMessage = "controlPlaneVIP is required to increase desired control-plane capacity above one; recreate the cluster with controlPlaneVIP configured"
	controlPlaneVIPUpgradeMessage  = "controlPlaneVIP is required to upgrade the control plane; recreate the cluster with controlPlaneVIP configured"
	controlPlaneVIPRolloutMessage  = "controlPlaneVIP is required to replace control-plane nodes; recreate the cluster with controlPlaneVIP configured before changing rollout-triggering control-plane configuration"
)

// validateControlPlaneCapacityChange lets existing no-VIP clusters keep or reduce capacity,
// but rejects growth beyond one control-plane node.
func validateControlPlaneCapacityChange(controlPlaneVIP string, current, desired int) error {
	if strings.TrimSpace(controlPlaneVIP) == "" && desired > 1 && desired > current {
		return newBadRequestErrorf("%s", controlPlaneVIPRequiredMessage)
	}
	return nil
}

func validateControlPlaneUpgrade(controlPlaneVIP string) error {
	if strings.TrimSpace(controlPlaneVIP) == "" {
		return newBadRequestErrorf("%s", controlPlaneVIPUpgradeMessage)
	}
	return nil
}

func validateControlPlaneRollout(controlPlaneVIP string, rolloutRequired bool) error {
	if strings.TrimSpace(controlPlaneVIP) == "" && rolloutRequired {
		return newBadRequestErrorf("%s", controlPlaneVIPRolloutMessage)
	}
	return nil
}

func desiredControlPlaneNodeCount(nodePools []db.NodePool) int {
	count := 0
	for i := range nodePools {
		if nodePools[i].ControlPlane {
			count += nodePools[i].DesiredCount
		}
	}
	return count
}
