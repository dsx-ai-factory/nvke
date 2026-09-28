// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

// OpenVSwitchConfig identifies the host interface moved onto an OVN bridge.
// A nil config preserves the legacy default-route selection.
type OpenVSwitchConfig struct {
	UplinkInterface string `json:"uplink_interface"`
	BridgeName      string `json:"bridge_name"`
}

// Clone copies the optional configuration for an immutable snapshot.
func (c *OpenVSwitchConfig) Clone() *OpenVSwitchConfig {
	if c == nil {
		return nil
	}
	cloned := *c
	return &cloned
}

// Equal reports whether the optional configurations select the same network.
func (c *OpenVSwitchConfig) Equal(other *OpenVSwitchConfig) bool {
	if c == nil || other == nil {
		return c == other
	}
	return *c == *other
}
