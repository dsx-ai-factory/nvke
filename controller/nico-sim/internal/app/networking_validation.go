// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/state"
)

// ValidateVPCAndSubnets validates persisted subnet and vpc-prefix ranges are
// non-empty, valid IPv4 CIDRs contained within bridgeCIDR.
func ValidateVPCAndSubnets(ctx context.Context, store *state.Store, bridgeCIDR string) error {
	_, bridgeNet, err := net.ParseCIDR(bridgeCIDR)
	if err != nil {
		return fmt.Errorf("invalid bridge cidr %q: %w", bridgeCIDR, err)
	}

	subnets, err := store.ListVMSubnets(ctx)
	if err != nil {
		return fmt.Errorf("failed listing subnets for cidr validation: %w", err)
	}
	prefixes, err := store.ListVMVPCPrefixes(ctx)
	if err != nil {
		return fmt.Errorf("failed listing vpc prefixes for cidr validation: %w", err)
	}
	return validateNetworkingRangesContained(bridgeNet, subnets, prefixes)
}

func validateNetworkingRangesContained(
	bridgeNet *net.IPNet,
	subnets []state.SubnetRow,
	prefixes []state.VPCPrefixRow,
) error {
	bridgeCIDR := bridgeNet.String()
	for _, subnet := range subnets {
		if strings.TrimSpace(subnet.CIDR) == "" {
			return fmt.Errorf("subnet %s has empty cidr", subnet.ID)
		}
		_, subnetNet, err := net.ParseCIDR(subnet.CIDR)
		if err != nil {
			return fmt.Errorf("subnet %s has invalid cidr %q: %w", subnet.ID, subnet.CIDR, err)
		}
		contained, err := cidrContainedBy(bridgeNet, subnetNet)
		if err != nil {
			return fmt.Errorf("failed validating subnet %s cidr containment: %w", subnet.ID, err)
		}
		if !contained {
			return fmt.Errorf(
				"subnet %s cidr %q is not contained within bridge cidr %q",
				subnet.ID,
				subnet.CIDR,
				bridgeCIDR,
			)
		}
	}
	for _, pfx := range prefixes {
		if strings.TrimSpace(pfx.Prefix) == "" {
			return fmt.Errorf("vpc prefix %s has empty prefix", pfx.ID)
		}
		_, pfxNet, err := net.ParseCIDR(pfx.Prefix)
		if err != nil {
			return fmt.Errorf("vpc prefix %s has invalid prefix %q: %w", pfx.ID, pfx.Prefix, err)
		}
		contained, err := cidrContainedBy(bridgeNet, pfxNet)
		if err != nil {
			return fmt.Errorf("failed validating vpc prefix %s containment: %w", pfx.ID, err)
		}
		if !contained {
			return fmt.Errorf(
				"vpc prefix %s %q is not contained within bridge cidr %q",
				pfx.ID,
				pfx.Prefix,
				bridgeCIDR,
			)
		}
	}
	return nil
}

type ipv4Range struct {
	start uint32
	end   uint32
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
