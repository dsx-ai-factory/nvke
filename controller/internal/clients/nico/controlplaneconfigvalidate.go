// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// ValidateControlPlaneConfigPayload validates spec's NICo payload content:
// no nil instance type ID in the preference list, and exactly one of
// SubnetIDs or VpcPrefixIDs is set.
func (*Client) ValidateControlPlaneConfigPayload(spec db.ControlPlaneConfigSpec) error {
	nicoProvider, err := spec.NicoProvider()
	if err != nil {
		return err
	}
	if err := ValidateInstanceTypePreferenceIDs(nicoProvider.InstanceTypePreference); err != nil {
		return err
	}
	return ValidateNetworkSelection(nicoProvider.SubnetIDs, nicoProvider.VpcPrefixIDs, "controlPlaneConfig.nico.")
}

// ValidateInstanceTypePreferenceIDs rejects a nil (all-zero) UUID in the
// control-plane instance type preference list. Exported because callers that
// validate this list before a full db.ControlPlaneConfigSpec exists (e.g. on
// create, before subnet/VPC-prefix fields are known) need it directly, not
// just through ValidateControlPlaneConfigPayload, which also enforces the
// subnet/VPC-prefix exclusivity rule.
func ValidateInstanceTypePreferenceIDs(ids []uuid.UUID) error {
	for _, id := range ids {
		if id == uuid.Nil {
			return fmt.Errorf("controlPlaneConfig.instanceTypePreference contains an invalid nil instance type ID")
		}
	}
	return nil
}
