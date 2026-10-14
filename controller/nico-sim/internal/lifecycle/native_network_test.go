// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"io"
	"log/slog"
	"testing"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/state"
	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/vm"
)

func TestNativeIPXENetworkSelection(t *testing.T) {
	for _, tc := range []struct {
		name, backend, handoff, libvirtNetwork string
		wantDHCP                               bool
	}{
		{"native explicit IPAM", config.VMBackendDarwinQEMU, "nvke-userdata", "", false},
		{"native seed has no IPAM override", config.VMBackendDarwinQEMU, "seed", "", true},
		{"Linux keeps DHCP", config.VMBackendLibvirt, "nvke-userdata", "", true},
		{"libvirt network keeps DHCP", config.VMBackendLibvirt, "seed", "default", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{VMBackend: tc.backend, GuestNetworkConfig: tc.handoff, LibvirtNetwork: tc.libvirtNetwork, BridgeCIDR: "172.30.0.0/24"}
			network := newNetworkManager(cfg, vm.NewMockRuntime())
			instance := state.Instance{IpxeScript: "#!ipxe\nimage-url http://172.30.0.1/exact.qcow2\n"}
			if got := network.NeedsDHCP(instance); got != tc.wantDHCP {
				t.Fatalf("iPXE address ownership differs: DHCP=%v, want %v", got, tc.wantDHCP)
			}
			if !tc.wantDHCP {
				if _, _, err := network.LookupLease(t.Context(), "00:11:22:33:44:55"); err == nil {
					t.Fatal("native IPAM silently accepted a DHCP lookup")
				}
				addrs := network.MetadataAddrs()
				if len(addrs) != 1 || addrs[0] != "172.30.0.1:7777" {
					t.Fatalf("native metadata endpoint differs: %v", addrs)
				}
			}
		})
	}
}

func TestNativeIPAMAddressAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, handoff, addresses, wantIP string
		wantError                        bool
	}{
		{"allocated iPXE address", "nvke-userdata", `["172.30.0.2"]`, "172.30.0.2", false},
		{"missing interface", "nvke-userdata", "", "", true},
		{"malformed interface", "nvke-userdata", `{}`, "", true},
		{"unadmitted native DHCP", "seed", `["172.30.0.2"]`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{VMBackend: config.VMBackendDarwinQEMU, GuestNetworkConfig: tc.handoff}
			engine := Engine{cfg: cfg, net: newNetworkManager(cfg, vm.NewMockRuntime())}
			instance := state.Instance{IpxeScript: "#!ipxe\nimage-url http://172.30.0.1/exact.qcow2\n"}
			if tc.addresses != "" {
				instance.Interfaces = []state.InstanceInterfaceRow{{IPAddressesJSON: tc.addresses}}
			}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			ip, err := engine.prepareInstanceNetwork(t.Context(), instance, "00:11:22:33:44:55", log)
			if (err != nil) != tc.wantError || ip != tc.wantIP {
				t.Fatalf("native address admission differs: IP=%q error=%v", ip, err)
			}
		})
	}
}
