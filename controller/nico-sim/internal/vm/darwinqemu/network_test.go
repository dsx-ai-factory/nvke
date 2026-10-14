// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package darwinqemu

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestNetworkArgs(t *testing.T) {
	tests := []struct {
		name string
		cidr string
		args []string
	}{
		{name: "default shared subnet", cidr: "192.168.64.0/24", args: []string{"--start-address", "192.168.64.1", "--end-address", "192.168.64.254", "--subnet-mask", "255.255.255.0"}},
		{name: "narrow subnet", cidr: "10.42.0.0/29", args: []string{"--start-address", "10.42.0.1", "--end-address", "10.42.0.6", "--subnet-mask", "255.255.255.248"}},
		{name: "host bits are normalized", cidr: "192.168.64.23/24", args: []string{"--start-address", "192.168.64.1", "--end-address", "192.168.64.254", "--subnet-mask", "255.255.255.0"}},
		{name: "invalid CIDR", cidr: "invalid"},
		{name: "IPv6 is unsupported", cidr: "fd00::/64"},
		{name: "public subnet", cidr: "203.0.113.0/24"},
		{name: "no usable guest addresses", cidr: "192.168.64.0/31"},
		{name: "oversized subnet", cidr: "10.0.0.0/8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, err := networkArgs(tt.cidr)
			if tt.args == nil {
				if err == nil || args != nil {
					t.Fatalf("unsupported network produced args: %v, %v", args, err)
				}
				return
			}
			if err != nil || !slices.Equal(args, tt.args) {
				t.Fatalf("network args = %v, %v; want %v", args, err, tt.args)
			}
		})
	}
}

func TestNetworkHelperIsJoined(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"vmnet_start_address\":\"192.168.64.1\",\"vmnet_subnet_mask\":\"255.255.255.0\"}'\nexec /bin/sleep 30\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	args, err := networkArgs("192.168.64.0/24")
	if err != nil {
		t.Fatal(err)
	}
	network, err := startNetwork(ctx, helper, dir, "192.168.64.0/24", args)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		_ = network.stop(cleanup)
	})
	cleanup, stop := context.WithTimeout(t.Context(), 2*time.Second)
	defer stop()
	if err := network.stop(cleanup); err != nil {
		t.Fatal(err)
	}
	select {
	case <-network.done:
	default:
		t.Fatal("network teardown returned before joining its helper")
	}
	if err := network.stop(cleanup); err != nil {
		t.Fatalf("repeated teardown: %v", err)
	}
}

func TestNetworkHelperAdmissionFailureJoinsChild(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "different subnet", payload: "{\"vmnet_start_address\":\"192.168.65.1\",\"vmnet_subnet_mask\":\"255.255.255.0\"}"},
		{name: "malformed description", payload: "invalid-json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			helper := filepath.Join(dir, "helper")
			if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf '%s\\n' '"+tt.payload+"'\nexec /bin/sleep 30\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			start := time.Now()
			network, err := startNetwork(ctx, helper, dir, "192.168.64.0/24", nil)
			if err == nil || network != nil {
				t.Fatalf("invalid helper was admitted: %v, %v", network, err)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("helper rejection did not complete bounded teardown")
			}
		})
	}
}
