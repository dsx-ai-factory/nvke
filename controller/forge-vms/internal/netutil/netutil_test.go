// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package netutil_test

import (
	"testing"

	"github.com/NVIDIA/nke/controller/forge-vms/internal/netutil"
)

func TestGatewayIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		cidr    string
		want    string
		wantErr bool
	}{
		{"192.168.200.0/24", "192.168.200.1", false},
		{"10.0.0.0/8", "10.0.0.1", false},
		{"172.16.0.0/16", "172.16.0.1", false},
		{"10.0.0.0/30", "10.0.0.1", false},
		// Host bits in CIDR input — net.ParseCIDR masks them off.
		{"192.168.200.5/24", "192.168.200.1", false},
		// Errors.
		{"not-a-cidr", "", true},
		{"", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.cidr, func(t *testing.T) {
			t.Parallel()
			got, err := netutil.GatewayIP(tc.cidr)
			if (err != nil) != tc.wantErr {
				t.Fatalf("GatewayIP(%q) error = %v, wantErr %v", tc.cidr, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("GatewayIP(%q) = %q, want %q", tc.cidr, got, tc.want)
			}
		})
	}
}

func TestCIDRPrefixLength(t *testing.T) {
	t.Parallel()

	tests := []struct {
		cidr string
		want int
	}{
		{"192.168.200.0/24", 24},
		{"10.0.0.0/8", 8},
		{"172.16.0.0/16", 16},
		{"0.0.0.0/0", 0},
		{"10.0.0.1/32", 32},
		// Invalid CIDR → defaults to 24.
		{"not-a-cidr", 24},
		{"", 24},
	}

	for _, tc := range tests {
		t.Run(tc.cidr, func(t *testing.T) {
			t.Parallel()
			got := netutil.CIDRPrefixLength(tc.cidr)
			if got != tc.want {
				t.Errorf("CIDRPrefixLength(%q) = %d, want %d", tc.cidr, got, tc.want)
			}
		})
	}
}
