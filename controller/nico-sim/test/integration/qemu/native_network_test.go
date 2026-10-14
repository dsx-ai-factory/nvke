//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qemu

import (
	"fmt"
	"os"
	"testing"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/netutil"
	"github.com/dsx-ai-factory/nvke/controller/nico-sim/test/integration/testutil"
)

// TestNativeNVKENetwork checks the explicitly selected NVKE image handoff.
// Console diagnostics avoid depending on SSH to diagnose a failed SSH probe.
func TestNativeNVKENetwork(t *testing.T) {
	if os.Getenv("NICO_VMS_NATIVE_NETWORK_DIAGNOSTIC") != "true" {
		t.Skip("requires explicit native network diagnostic admission")
	}
	env, profile := sharedSuite(t)
	env = env.WithT(t)
	gateway, err := netutil.GatewayIP(profile.Config.BridgeCIDR)
	if err != nil {
		t.Fatal(err)
	}
	userData := fmt.Sprintf(`#cloud-config
runcmd:
  - [sh, -c, 'exec > /dev/ttyAMA0 2>&1; echo NVKE_NETWORK_DIAGNOSTIC_BEGIN; ip -4 address show; ip route show; netplan get; systemctl is-active ssh; ss -lnt; ls /dev/tpm*; iptables -S; nft list ruleset; ping -c 1 -W 2 %s; curl --max-time 3 -s -o /dev/null -w "metadata-http-status=%%{http_code}\n" http://%s:7777/; echo NVKE_NETWORK_DIAGNOSTIC_END']
`, gateway, gateway)
	created := env.CreateInstance(&forgesdk.InstanceCreateRequest{
		Name:           testutil.UniqueInstanceName("native-network"),
		TenantId:       "d282929f-49d5-4dec-8d27-21ceb30ade19",
		InstanceTypeId: forgesdk.PtrString("itype-mock-001"),
		VpcId:          "vpc-mock-001",
		UserData:       *forgesdk.NewNullableString(forgesdk.PtrString(userData)),
		Interfaces:     []forgesdk.InterfaceCreateRequest{{SubnetId: forgesdk.PtrString("subnet-mock-001")}},
	})
	t.Cleanup(func() {
		testutil.PreserveInstanceArtifacts(t, profile, created.GetId())
		testutil.CleanupInstanceByID(t, env, profile, created.GetId())
	})
	ready := testutil.WaitForInstanceState(t, env, profile, created.GetId(), "ready")
	if ready.GetTpmEkCertificate() == "" {
		t.Fatal("native NVKE fixture requires a TPM EK certificate")
	}
	t.Log("native NVKE fixture passed the SSH readiness probe with a TPM certificate")
}
