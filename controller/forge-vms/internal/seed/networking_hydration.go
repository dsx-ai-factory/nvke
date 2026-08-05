// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

// Package seed loads and normalizes forge-vms seed documents.
package seed

import (
	"encoding/binary"
	"fmt"
	"net"
	"sort"
	"strings"
)

// HydrateNetworkingCIDRs fills missing subnet/prefix ranges from bridgeCIDR.
//
// This is a write-path helper used before ApplySeed so initial network ranges
// are derived from runtime config once. Existing persisted rows remain stable
// because ApplySeed upserts without mutating non-key columns.
func HydrateNetworkingCIDRs(doc Document, bridgeCIDR string) (Document, error) {
	trimmedBridge := strings.TrimSpace(bridgeCIDR)
	if trimmedBridge == "" {
		return Document{}, fmt.Errorf("bridge cidr is required for seed hydration")
	}
	_, bridgeNet, err := net.ParseCIDR(trimmedBridge)
	if err != nil {
		return Document{}, fmt.Errorf("invalid bridge cidr %q: %w", trimmedBridge, err)
	}

	if err := hydrateSubnets(&doc, trimmedBridge); err != nil {
		return Document{}, err
	}
	if err := hydrateVPCPrefixes(&doc, bridgeNet); err != nil {
		return Document{}, err
	}
	return doc, nil
}

func hydrateSubnets(doc *Document, bridgeCIDR string) error {
	for i := range doc.Subnets {
		subnet := &doc.Subnets[i]
		if strings.TrimSpace(subnet.CIDR) == "" {
			subnet.CIDR = bridgeCIDR
		}
		_, subnetNet, err := net.ParseCIDR(subnet.CIDR)
		if err != nil {
			return fmt.Errorf("seed subnet %s has invalid cidr %q: %w", subnet.ID, subnet.CIDR, err)
		}
		subnetPrefixLen, _ := subnetNet.Mask.Size()
		if subnet.PrefixLength <= 0 || subnet.PrefixLength != subnetPrefixLen {
			subnet.PrefixLength = subnetPrefixLen
		}
	}
	return nil
}

func hydrateVPCPrefixes(doc *Document, bridgeNet *net.IPNet) error {
	if len(doc.VPCPrefixes) == 0 {
		return nil
	}

	bridgePrefixLen, _ := bridgeNet.Mask.Size()
	usedRanges := make([]ipv4Range, 0, len(doc.VPCPrefixes))

	// Preserve explicitly configured prefixes and reserve their space.
	for i := range doc.VPCPrefixes {
		pfx := &doc.VPCPrefixes[i]
		if strings.TrimSpace(pfx.Prefix) == "" {
			continue
		}

		_, pfxNet, err := net.ParseCIDR(pfx.Prefix)
		if err != nil {
			return fmt.Errorf("seed vpc prefix %s has invalid prefix %q: %w", pfx.ID, pfx.Prefix, err)
		}
		pfxPrefixLen, _ := pfxNet.Mask.Size()
		if pfx.PrefixLength <= 0 || pfx.PrefixLength != pfxPrefixLen {
			pfx.PrefixLength = pfxPrefixLen
		}

		contained, err := cidrContainedBy(bridgeNet, pfxNet)
		if err != nil {
			return fmt.Errorf("failed validating seed vpc prefix %s containment: %w", pfx.ID, err)
		}
		if !contained {
			return fmt.Errorf(
				"seed vpc prefix %s %q is not contained within bridge cidr %q",
				pfx.ID,
				pfx.Prefix,
				bridgeNet.String(),
			)
		}

		pfxRange, err := ipv4RangeFromIPNet(pfxNet)
		if err != nil {
			return fmt.Errorf("failed resolving seed vpc prefix %s range: %w", pfx.ID, err)
		}
		for _, used := range usedRanges {
			if rangesOverlap(used, pfxRange) {
				return fmt.Errorf(
					"seed vpc prefix %s overlaps another configured vpc prefix (%s)",
					pfx.ID,
					pfx.Prefix,
				)
			}
		}
		usedRanges = append(usedRanges, pfxRange)
	}

	missingIdx := make([]int, 0, len(doc.VPCPrefixes))
	for i, pfx := range doc.VPCPrefixes {
		if strings.TrimSpace(pfx.Prefix) == "" {
			missingIdx = append(missingIdx, i)
		}
	}
	sort.SliceStable(missingIdx, func(i, j int) bool {
		return doc.VPCPrefixes[missingIdx[i]].ID < doc.VPCPrefixes[missingIdx[j]].ID
	})

	for _, idx := range missingIdx {
		pfx := &doc.VPCPrefixes[idx]
		prefixLen := normalizePrefixLength(pfx.PrefixLength, bridgePrefixLen)
		prefixCIDR, pfxRange, err := nextAvailablePrefixCIDR(bridgeNet, prefixLen, usedRanges)
		if err != nil {
			return fmt.Errorf("failed hydrating seed vpc prefix %s: %w", pfx.ID, err)
		}
		pfx.Prefix = prefixCIDR
		pfx.PrefixLength = prefixLen
		usedRanges = append(usedRanges, pfxRange)
	}
	return nil
}

type ipv4Range struct {
	start uint32
	end   uint32
}

func normalizePrefixLength(requestedPrefixLen, bridgePrefixLen int) int {
	if requestedPrefixLen <= 0 {
		preferred := bridgePrefixLen + 5
		if preferred > 30 {
			preferred = 30
		}
		if preferred < bridgePrefixLen {
			preferred = bridgePrefixLen
		}
		return preferred
	}
	if requestedPrefixLen < bridgePrefixLen || requestedPrefixLen > 30 {
		return bridgePrefixLen
	}
	return requestedPrefixLen
}

func nextAvailablePrefixCIDR(bridgeNet *net.IPNet, prefixLen int, usedRanges []ipv4Range) (string, ipv4Range, error) {
	bridgeRange, err := ipv4RangeFromIPNet(bridgeNet)
	if err != nil {
		return "", ipv4Range{}, err
	}
	blockSize := uint32(1) << uint(32-prefixLen)
	for start := int64(bridgeRange.end) - int64(blockSize) + 1; start >= int64(bridgeRange.start); start -= int64(blockSize) {
		candidate := ipv4Range{
			start: uint32(start),
			end:   uint32(start) + blockSize - 1,
		}
		overlap := false
		for _, used := range usedRanges {
			if rangesOverlap(used, candidate) {
				overlap = true
				break
			}
		}
		if overlap {
			continue
		}
		out := make([]byte, net.IPv4len)
		binary.BigEndian.PutUint32(out, candidate.start)
		return fmt.Sprintf("%s/%d", net.IP(out).String(), prefixLen), candidate, nil
	}
	return "", ipv4Range{}, fmt.Errorf("unable to place vpc prefix /%d inside bridge cidr %s", prefixLen, bridgeNet.String())
}

func rangesOverlap(a, b ipv4Range) bool {
	return a.start <= b.end && b.start <= a.end
}

func cidrContainedBy(parent, child *net.IPNet) (bool, error) {
	parentRange, err := ipv4RangeFromIPNet(parent)
	if err != nil {
		return false, err
	}
	childRange, err := ipv4RangeFromIPNet(child)
	if err != nil {
		return false, err
	}
	return childRange.start >= parentRange.start && childRange.end <= parentRange.end, nil
}

func ipv4RangeFromIPNet(ipNet *net.IPNet) (ipv4Range, error) {
	baseIP := ipNet.IP.To4()
	if baseIP == nil {
		return ipv4Range{}, fmt.Errorf("cidr must be ipv4")
	}
	mask := ipNet.Mask
	if len(mask) != net.IPv4len {
		return ipv4Range{}, fmt.Errorf("cidr must be ipv4")
	}
	network := binary.BigEndian.Uint32(baseIP) & binary.BigEndian.Uint32(mask)
	broadcast := network | ^binary.BigEndian.Uint32(mask)
	return ipv4Range{start: network, end: broadcast}, nil
}
