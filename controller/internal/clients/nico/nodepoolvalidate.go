// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// ValidateNodePoolPayload validates nodePool's NICo payload content: the
// disk selection strategy is a recognized value, and exactly one of
// SubnetIDs or VpcPrefixIDs is set.
func (*Client) ValidateNodePoolPayload(nodePool db.NodePool) error {
	nicoNodePoolProvider, err := nodePool.NicoProvider()
	if err != nil {
		return err
	}
	if err := ValidateDiskSelectionStrategy(nicoNodePoolProvider.DiskSelectionStrategy); err != nil {
		return err
	}
	return ValidateNetworkSelection(nicoNodePoolProvider.SubnetIDs, nicoNodePoolProvider.VpcPrefixIDs, "nico.")
}

// ValidateDiskSelectionStrategy checks s is a recognized disk selection
// strategy value. Exported because update flows validate it against the
// merged node pool without also re-enforcing subnet/VPC-prefix exclusivity,
// which (unlike on create) update is allowed to leave both empty.
func ValidateDiskSelectionStrategy(s db.DiskSelectionStrategy) error {
	switch s {
	case "", db.DiskSelectionStrategySmallest, db.DiskSelectionStrategyLargest:
		return nil
	default:
		return fmt.Errorf("invalid nico.diskSelectionStrategy: %q", s)
	}
}

// ValidateNetworkSelection requires exactly one of subnetIDs or vpcPrefixIDs
// to be set. fieldPrefix names the request field this error maps back to
// (e.g. "nico." or "controlPlaneConfig.nico."). Exported because callers that
// validate subnet/VPC-prefix exclusivity against resolved Forge-site data
// before a full db.NodePool/db.ControlPlaneConfigSpec exists need it
// directly, not just through ValidateNodePoolPayload/
// ValidateControlPlaneConfigPayload.
func ValidateNetworkSelection(subnetIDs, vpcPrefixIDs []uuid.UUID, fieldPrefix string) error {
	hasSubnetIDs := len(subnetIDs) > 0
	hasVpcPrefixIDs := len(vpcPrefixIDs) > 0
	if hasSubnetIDs == hasVpcPrefixIDs {
		return fmt.Errorf("exactly one of %ssubnetIDs or %svpcPrefixIDs must be provided", fieldPrefix, fieldPrefix)
	}
	return nil
}
