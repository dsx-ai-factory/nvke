// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vm

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os/exec"
	"strings"
)

// EnsureLinuxBridge creates a Linux bridge if it does not exist, assigns the
// gateway IP (network+1) derived from bridgeCIDR, and brings the interface up.
func EnsureLinuxBridge(ctx context.Context, bridgeName, bridgeCIDR string) error {
	// Create bridge if it doesn't exist.
	if _, err := exec.CommandContext(ctx, "ip", "link", "show", bridgeName).CombinedOutput(); err != nil {
		if out, createErr := exec.CommandContext(ctx, "ip", "link", "add", "name", bridgeName, "type", "bridge").CombinedOutput(); createErr != nil {
			return fmt.Errorf("failed to create bridge %s: %w: %s", bridgeName, createErr, string(out))
		}
	}

	// Assign gateway address.
	gwCIDR, err := gatewayCIDRFromBridge(bridgeCIDR)
	if err != nil {
		return err
	}

	out, err := exec.CommandContext(ctx, "ip", "addr", "add", gwCIDR, "dev", bridgeName).CombinedOutput()
	if err != nil {
		outStr := string(out)
		if !strings.Contains(outStr, "File exists") && !strings.Contains(outStr, "Address already assigned") {
			return fmt.Errorf("failed to assign bridge address %s: %w: %s", gwCIDR, err, outStr)
		}
	}

	// Bring bridge up.
	if out, err := exec.CommandContext(ctx, "ip", "link", "set", bridgeName, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("failed to bring bridge %s up: %w: %s", bridgeName, err, string(out))
	}

	return nil
}

func gatewayCIDRFromBridge(cidr string) (string, error) {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parse CIDR %s: %w", cidr, err)
	}
	v4 := ip.To4()
	if v4 == nil {
		return "", fmt.Errorf("only IPv4 CIDRs are supported")
	}
	mask := ipNet.Mask
	network := binary.BigEndian.Uint32(v4) & binary.BigEndian.Uint32(mask)
	prefixLen, _ := mask.Size()
	gw := make([]byte, 4)
	binary.BigEndian.PutUint32(gw, network+1)
	return fmt.Sprintf("%s/%d", net.IP(gw).String(), prefixLen), nil
}
