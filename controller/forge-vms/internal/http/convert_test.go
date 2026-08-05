// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package http

import (
	"testing"

	"github.com/NVIDIA/nke/controller/forge-vms/internal/state"
)

func TestToSDKSubnetSeparatesIPv4PrefixAndLength(t *testing.T) {
	subnet := toSDKSubnet(state.SubnetRow{
		CIDR:         "192.168.200.0/24",
		PrefixLength: 24,
	})

	if got := subnet.GetIpv4Prefix(); got != "192.168.200.0" {
		t.Fatalf("IPv4 prefix = %q, want %q", got, "192.168.200.0")
	}
	if got := subnet.GetPrefixLength(); got != 24 {
		t.Fatalf("prefix length = %d, want 24", got)
	}
}
