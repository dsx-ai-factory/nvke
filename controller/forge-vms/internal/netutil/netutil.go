// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

// Package netutil provides shared IP/CIDR utilities used across forge-vms packages.
package netutil

import (
	"encoding/binary"
	"fmt"
	"net"
)

// GatewayIP returns the first usable IP (network+1) from a CIDR string.
func GatewayIP(cidr string) (string, error) {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("invalid CIDR %q: %w", cidr, err)
	}
	v4 := ip.To4()
	if v4 == nil {
		return "", fmt.Errorf("CIDR %q is not IPv4", cidr)
	}
	mask := ipNet.Mask
	if len(mask) != 4 {
		return "", fmt.Errorf("CIDR %q has unexpected mask length %d", cidr, len(mask))
	}
	network := binary.BigEndian.Uint32(v4) & binary.BigEndian.Uint32(mask)
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, network+1)
	return net.IP(b).String(), nil
}

// CIDRPrefixLength returns the prefix length of a CIDR, defaulting to 24 on error.
func CIDRPrefixLength(cidr string) int {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return 24
	}
	prefixLen, _ := ipNet.Mask.Size()
	return prefixLen
}
