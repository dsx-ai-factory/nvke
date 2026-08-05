// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package ipam

import (
	"encoding/binary"
	"fmt"
	"net"
)

func FirstFreeIPv4(cidr string, count int, used map[string]struct{}) ([]string, error) {
	if count <= 0 {
		return nil, fmt.Errorf("count must be > 0")
	}

	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("invalid CIDR %q: %w", cidr, err)
	}
	base := ip.To4()
	if base == nil {
		return nil, fmt.Errorf("only IPv4 CIDRs are supported")
	}
	mask := ipNet.Mask
	if len(mask) != 4 {
		return nil, fmt.Errorf("invalid IPv4 mask for CIDR %q", cidr)
	}

	network := binary.BigEndian.Uint32(base) & binary.BigEndian.Uint32(mask)
	broadcast := network | ^binary.BigEndian.Uint32(mask)
	gateway := network + 1

	out := make([]string, 0, count)
	for curr := network + 1; curr < broadcast && len(out) < count; curr++ {
		if curr == gateway {
			continue
		}

		candidate := uint32ToIPv4String(curr)
		if _, exists := used[candidate]; exists {
			continue
		}

		out = append(out, candidate)
		used[candidate] = struct{}{}
	}

	if len(out) < count {
		return nil, fmt.Errorf("insufficient address space in %s", cidr)
	}

	return out, nil
}

func uint32ToIPv4String(v uint32) string {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return net.IP(b).String()
}
