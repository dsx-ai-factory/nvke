// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification_test

import (
	"os"
	"testing"
)

// TestDSXRecipeValidation reserves the RelEng acceptance suite from section 10
// of the RelEng recipe validation requirements.
func TestDSXRecipeValidation(t *testing.T) {
	pendingUbuntuVersions(t, "run RelEng's section 10 acceptance criteria on the shared DSX pool and record the qualification results")
}

// TestDSXHardwareSmoke checks candidate images on explicitly supplied nodes.
func TestDSXHardwareSmoke(t *testing.T) {
	configPath := os.Getenv("DSX_SMOKE_CONFIG")
	if configPath == "" {
		pendingUbuntuVersions(t, "set DSX_SMOKE_CONFIG to check provisioned nodes; unattended NICo provisioning is not implemented")
		return
	}
	cfg, err := loadSmokeConfig(configPath, os.Getenv("DSX_MACHINE_TYPE_ID"))
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range cfg.Targets {
		t.Run("ubuntu-"+target.Expected.Ubuntu, func(t *testing.T) {
			facts, err := probeSSH(t.Context(), target)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkSmoke(target, facts); err != nil {
				t.Fatal(err)
			}
			t.Logf("Host checks passed for build %s; allocation and image artifact verification are outside this test", target.Expected.BuildID)
		})
	}
}

// TestDSXFDEEndToEnd reserves the real-TPM FDE provisioning and cluster checks.
func TestDSXFDEEndToEnd(t *testing.T) {
	pendingUbuntuVersions(t, "verify the NICo iPXE key handoff, real TPM attestation and binding, encrypted root, and FDE-backed cluster/workload health")
}

// TestDSXUpgradeAndReboot reserves node lifecycle checks for ordinary and FDE images.
func TestDSXUpgradeAndReboot(t *testing.T) {
	pendingUbuntuVersions(t, "verify reboot and power cycle without reimage, N-1 to N upgrade within an Ubuntu series, state preservation, FDE unseal, and recovered node/workload health")
}

// pendingUbuntuVersions keeps the epic's two required Ubuntu versions inside
// each functional test. Skipped cases are not qualification evidence.
func pendingUbuntuVersions(t *testing.T, acceptance string) {
	t.Helper()
	if machineType := os.Getenv("DSX_MACHINE_TYPE_ID"); machineType != "" {
		t.Logf("Requested NICo instance type: %s", machineType)
	}
	for _, ubuntu := range []string{"24.04", "26.04"} {
		t.Run("ubuntu-"+ubuntu, func(t *testing.T) {
			t.Skip("not implemented: " + acceptance)
		})
	}
}
